// Command watcher is the Caw Watcher: an MCP (Model Context Protocol) server
// that an agent drives over stdio. It bridges the agent to a running Hub,
// exposing three tools (Slice 4, ADR-0005/0006):
//
//   - subscribe_pr         — open an SSE stream for one PR and return the
//     compiled summaries received within a bounded window, rendered with
//     accessible severity symbols (via internal/severity).
//   - get_pending          — one-shot fetch of every current Pending item.
//   - acquire_rebase_lease — request a force-push lease from the Hub (ADR-0005).
//     Rebase EXECUTION, heartbeat-during-rebase, and orphan fallback are
//     Slice 6 — explicitly OUT OF SCOPE here. See TODO(#6) below.
//
// Auth: every Hub call carries the Hub-minted installation token (ADR-0003),
// read from the environment (CAW_WATCHER_HUB_URL / CAW_WATCHER_TOKEN).
//
// Transport note: stdout is reserved for the MCP JSON-RPC stream, so all
// diagnostics go to stderr via the standard logger.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ravencloak-org/caw/internal/watcher"
)

// version is the MCP server's advertised implementation version.
const version = "0.1.0"

// subscribePollWindow bounds how long subscribe_pr holds the SSE stream during
// a single tool call before returning whatever summaries arrived. An MCP tool
// call is request/response, so the long-lived stream is sampled, not held open
// forever. Agents re-invoke subscribe_pr to keep watching.
const subscribePollWindow = 25 * time.Second

func main() {
	// Logs must not pollute stdout (the MCP transport); send them to stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("watcher: ")
	log.SetFlags(0)

	cfg, err := watcher.ConfigFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	client := watcher.NewClient(cfg.HubURL, cfg.Token)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := newServer(client)

	log.Printf("starting MCP stdio server (hub=%s)", cfg.HubURL)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("run: %v", err)
	}
}

// newServer builds the MCP server and registers the three Watcher tools plus
// the session-start prompt. Split out from main so tests can construct it
// without touching stdio.
func newServer(client *watcher.Client) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "caw-watcher", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_pending",
		Description: "Fetch every PR currently waiting for attention on the Hub " +
			"(orphaned or open feedback). Call this at the start of a fresh session.",
	}, makeGetPending(client))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "subscribe_pr",
		Description: "Open a live stream for one pull request and return the " +
			"compiled review summaries that arrive in the next ~25s, rendered with " +
			"accessible severity symbols. Re-invoke to keep watching.",
	}, makeSubscribePR(client))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "acquire_rebase_lease",
		Description: "Request the single-owner force-push lease for a PR (ADR-0005). " +
			"Returns whether the lease was granted and who currently holds it. " +
			"Does NOT perform the rebase itself.",
	}, makeAcquireRebaseLease(client))

	// SessionStart reminder surfaced as an MCP prompt the agent can fetch. The
	// Claude Code harness also fires a SessionStart hook (.claude/settings.json)
	// that injects the same nudge.
	srv.AddPrompt(&mcp.Prompt{
		Name:        "session_start",
		Description: "Reminder to check pending PRs at the start of a session.",
	}, sessionStartPrompt)

	return srv
}

// --- tool input/output types (drive auto-generated JSON schemas) ---

// prRef identifies a pull request for subscribe_pr and acquire_rebase_lease.
type prRef struct {
	Owner  string `json:"owner" jsonschema:"the repository owner / org login"`
	Repo   string `json:"repo" jsonschema:"the repository name"`
	Number int    `json:"number" jsonschema:"the pull request number"`
}

// emptyInput is used by tools that take no arguments.
type emptyInput struct{}

// pendingOutput is the structured result of get_pending.
type pendingOutput struct {
	Items []watcher.PendingItem `json:"items"`
}

// subscribeOutput is the structured result of subscribe_pr.
type subscribeOutput struct {
	Key       string                   `json:"key"`
	Summaries []watcher.SummaryMessage `json:"summaries"`
}

// leaseOutput is the structured result of acquire_rebase_lease.
type leaseOutput struct {
	watcher.LeaseResult
}

// --- tool handlers ---

func makeGetPending(client *watcher.Client) mcp.ToolHandlerFor[emptyInput, pendingOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, pendingOutput, error) {
		items, err := client.GetPending(ctx)
		if err != nil {
			return nil, pendingOutput{}, fmt.Errorf("get_pending: %w", err)
		}
		out := pendingOutput{Items: items}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: renderPending(items)}},
		}, out, nil
	}
}

func makeSubscribePR(client *watcher.Client) mcp.ToolHandlerFor[prRef, subscribeOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in prRef) (*mcp.CallToolResult, subscribeOutput, error) {
		// Bound the stream to a single poll window so the tool call returns.
		streamCtx, cancel := context.WithTimeout(ctx, subscribePollWindow)
		defer cancel()

		var msgs []watcher.SummaryMessage
		err := client.SubscribePR(streamCtx, in.Owner, in.Repo, in.Number, func(m watcher.SummaryMessage) {
			msgs = append(msgs, m)
		})
		// A timeout or canceled context is the expected, graceful end of a poll
		// window — not a failure. Surface only genuine transport errors.
		if err != nil && streamCtx.Err() == nil {
			return nil, subscribeOutput{}, fmt.Errorf("subscribe_pr: %w", err)
		}

		out := subscribeOutput{
			Key:       fmt.Sprintf("%s/%s#%d", in.Owner, in.Repo, in.Number),
			Summaries: msgs,
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: renderSubscribe(out)}},
		}, out, nil
	}
}

func makeAcquireRebaseLease(client *watcher.Client) mcp.ToolHandlerFor[prRef, leaseOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in prRef) (*mcp.CallToolResult, leaseOutput, error) {
		res, err := client.AcquireRebaseLease(ctx, in.Owner, in.Repo, in.Number)
		if err != nil {
			return nil, leaseOutput{}, fmt.Errorf("acquire_rebase_lease: %w", err)
		}
		// TODO(#6): Slice 6 owns rebase EXECUTION, heartbeat-during-rebase, and
		// orphan fallback. This tool only secures the lease; once granted, the
		// agent must NOT yet attempt a force-push from here.
		out := leaseOutput{LeaseResult: res}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: renderLease(in, res)}},
		}, out, nil
	}
}

// sessionStartPrompt returns the canned reminder to check pending PRs.
func sessionStartPrompt(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	return &mcp.GetPromptResult{
		Description: "Caw watcher session start",
		Messages: []*mcp.PromptMessage{
			{
				Role: "user",
				Content: &mcp.TextContent{
					Text: "You just started a fresh session. Call the `get_pending` " +
						"tool to see which pull requests are waiting for attention before " +
						"doing anything else.",
				},
			},
		},
	}, nil
}

// --- human-readable rendering for the text content block ---

func renderPending(items []watcher.PendingItem) string {
	if len(items) == 0 {
		return "No pending PRs."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d pending PR(s):\n", len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "- %s/%s#%d [%s] %s\n", it.Owner, it.Repo, it.Number, it.SignalType, it.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderSubscribe(out subscribeOutput) string {
	if len(out.Summaries) == 0 {
		return fmt.Sprintf("No new summaries for %s in this window.", out.Key)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d summary update(s) for %s:\n", len(out.Summaries), out.Key)
	for _, m := range out.Summaries {
		fmt.Fprintf(&b, "── seq %d ──\n%s\n", m.Seq, m.Rendered)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderLease(in prRef, res watcher.LeaseResult) string {
	target := fmt.Sprintf("%s/%s#%d", in.Owner, in.Repo, in.Number)
	if res.Granted {
		return fmt.Sprintf("Rebase lease GRANTED for %s (holder=%s, expires_at=%d).",
			target, res.Holder, res.ExpiresAt)
	}
	return fmt.Sprintf("Rebase lease DENIED for %s; currently held by %s (expires_at=%d).",
		target, res.Holder, res.ExpiresAt)
}
