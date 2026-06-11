// Command watcher is the Caw Watcher: an MCP (Model Context Protocol) server
// that an agent drives over stdio. It bridges the agent to a running Hub,
// exposing six tools (Slice 4 + Slice 6, ADR-0005/0006):
//
//   - subscribe_pr          — open an SSE stream for one PR and return the
//     compiled summaries received within a bounded window, rendered with
//     accessible severity symbols (via internal/severity).
//   - get_pending           — one-shot fetch of every current Pending item.
//   - acquire_rebase_lease  — request a force-push lease from the Hub (ADR-0005).
//   - renew_rebase_lease    — heartbeat an active lease to prevent expiry (ADR-0005).
//   - release_rebase_lease  — release the lease when the agent is done.
//   - rebase_pr             — perform the full rebase with heartbeat under an
//     already-acquired lease (Slice 6, ADR-0002/0005).
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

	"github.com/ravencloak-org/caw/internal/rebase"
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
			"Call rebase_pr after a successful acquire to perform the rebase.",
	}, makeAcquireRebaseLease(client))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "renew_rebase_lease",
		Description: "Send a heartbeat to extend an active force-push lease (ADR-0005). " +
			"Call every ~30 s while a long-running rebase is in progress to prevent expiry.",
	}, makeRenewRebaseLease(client))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "release_rebase_lease",
		Description: "Release the force-push lease when the rebase is complete or aborted (ADR-0005). " +
			"Always call this when done, even after an error.",
	}, makeReleaseRebaseLease(client))

	mcp.AddTool(srv, &mcp.Tool{
		Name: "rebase_pr",
		Description: "Perform the full rebase sequence (fetch / rebase / force-push) for a PR " +
			"under an already-acquired Hub lease, with automatic heartbeats every 30 s (Slice 6, ADR-0002). " +
			"Requires owner, repo, number, sha (HEAD commit), branch, remote, base, and holder (lease holder ID). " +
			"The git repository must be checked out in the directory specified by repo_dir.",
	}, makeRebasePR(client))

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

// leaseHolderInput carries the lease-holder identity for renew / release.
type leaseHolderInput struct {
	Owner  string `json:"owner" jsonschema:"the repository owner / org login"`
	Repo   string `json:"repo" jsonschema:"the repository name"`
	Number int    `json:"number" jsonschema:"the pull request number"`
	Holder string `json:"holder" jsonschema:"the lease holder ID returned by acquire_rebase_lease"`
}

// rebasePRInput is the full input for rebase_pr.
type rebasePRInput struct {
	Owner   string `json:"owner" jsonschema:"the repository owner / org login"`
	Repo    string `json:"repo" jsonschema:"the repository name"`
	Number  int    `json:"number" jsonschema:"the pull request number"`
	SHA     string `json:"sha" jsonschema:"the HEAD commit SHA being rebased"`
	Branch  string `json:"branch" jsonschema:"local branch name (e.g. 'feature/my-pr')"`
	Remote  string `json:"remote" jsonschema:"git remote name (usually 'origin')"`
	Base    string `json:"base" jsonschema:"upstream ref to rebase onto (e.g. 'origin/main')"`
	Holder  string `json:"holder" jsonschema:"the lease holder ID returned by acquire_rebase_lease"`
	RepoDir string `json:"repo_dir" jsonschema:"absolute path to the local git repository"`
}

// rebasePROutput is the structured result of rebase_pr.
type rebasePROutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
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
		out := leaseOutput{LeaseResult: res}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: renderLease(in, res)}},
		}, out, nil
	}
}

func makeRenewRebaseLease(client *watcher.Client) mcp.ToolHandlerFor[leaseHolderInput, leaseOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in leaseHolderInput) (*mcp.CallToolResult, leaseOutput, error) {
		// watcher.Client.RenewRebaseLease returns (LeaseResult, error).
		// We surface both in the output, but only return an error on failure.
		res, err := client.RenewRebaseLease(ctx, in.Owner, in.Repo, in.Number, in.Holder)
		if err != nil {
			return nil, leaseOutput{}, fmt.Errorf("renew_rebase_lease: %w", err)
		}
		out := leaseOutput{LeaseResult: res}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Lease renewed for %s/%s#%d (holder=%s)", in.Owner, in.Repo, in.Number, in.Holder),
			}},
		}, out, nil
	}
}

func makeReleaseRebaseLease(client *watcher.Client) mcp.ToolHandlerFor[leaseHolderInput, leaseOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in leaseHolderInput) (*mcp.CallToolResult, leaseOutput, error) {
		if err := client.ReleaseRebaseLease(ctx, in.Owner, in.Repo, in.Number, in.Holder); err != nil {
			return nil, leaseOutput{}, fmt.Errorf("release_rebase_lease: %w", err)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Lease released for %s/%s#%d (holder=%s)", in.Owner, in.Repo, in.Number, in.Holder),
			}},
		}, leaseOutput{}, nil
	}
}

func makeRebasePR(client *watcher.Client) mcp.ToolHandlerFor[rebasePRInput, rebasePROutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in rebasePRInput) (*mcp.CallToolResult, rebasePROutput, error) {
		cfg := rebase.Config{
			Owner:  in.Owner,
			Repo:   in.Repo,
			Number: in.Number,
			SHA:    in.SHA,
			Branch: in.Branch,
			Remote: in.Remote,
			Base:   in.Base,
			Holder: in.Holder,
		}

		// Thin adapter: watcher.Client.RenewRebaseLease returns (LeaseResult, error)
		// but rebase.LeaseRenewer requires only error. Wrap it here to satisfy the
		// interface without modifying either package.
		renewer := &clientLeaseAdapter{client: client}

		runner := rebase.NewExecRunner(in.RepoDir)
		session := rebase.NewSession(runner, renewer)

		if err := session.Run(ctx, cfg); err != nil {
			out := rebasePROutput{Success: false, Message: err.Error()}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{
					Text: fmt.Sprintf("Rebase failed for %s/%s#%d: %v", in.Owner, in.Repo, in.Number, err),
				}},
			}, out, err
		}

		out := rebasePROutput{Success: true, Message: "rebase complete"}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Rebase complete for %s/%s#%d", in.Owner, in.Repo, in.Number),
			}},
		}, out, nil
	}
}

// clientLeaseAdapter adapts *watcher.Client to rebase.LeaseRenewer.
// watcher.Client.RenewRebaseLease returns (LeaseResult, error); the interface
// only needs error.
type clientLeaseAdapter struct {
	client *watcher.Client
}

func (a *clientLeaseAdapter) RenewRebaseLease(ctx context.Context, owner, repo string, number int, holder string) error {
	_, err := a.client.RenewRebaseLease(ctx, owner, repo, number, holder)
	return err
}

func (a *clientLeaseAdapter) ReleaseRebaseLease(ctx context.Context, owner, repo string, number int, holder string) error {
	return a.client.ReleaseRebaseLease(ctx, owner, repo, number, holder)
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
