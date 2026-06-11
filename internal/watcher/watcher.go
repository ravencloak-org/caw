// Package watcher implements the Watcher MCP server (Slice 4, ADR-0005/0006).
// It exposes three MCP tools to agents:
//
//   - subscribe_pr   — holds an SSE stream from the Hub for a specific PR,
//     delivering compiled summaries as they arrive (rendered via internal/severity).
//   - get_pending    — one-shot fetch of all current Pending items from the Hub.
//   - acquire_rebase_lease — requests a force-push lease from the Hub (ADR-0005);
//     rebase execution and heartbeat are Slice 6 — TODO(#6).
//
// Auth: every Hub call carries the Hub-minted installation token (ADR-0003),
// supplied via watcher config/env.
package watcher

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ravencloak-org/caw/internal/compile"
	"github.com/ravencloak-org/caw/internal/severity"
)

// --- types exported for tests / MCP tool layer ---

// PendingItem mirrors store.PendingItem for JSON decoding of GET /pending.
type PendingItem struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	SignalType string `json:"signal_type"`
	SHA        string `json:"sha"`
	PRState    string `json:"pr_state"`
	Summary    string `json:"summary"`
	UpdatedAt  int64  `json:"updated_at"`
}

// LeaseResult is the decoded response from POST /leases/:owner/:repo/:number.
type LeaseResult struct {
	Granted         bool   `json:"granted"`
	Holder          string `json:"holder"`
	ExpiresAt       int64  `json:"expires_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	AcquiredAt      int64  `json:"acquired_at"`
}

// SummaryMessage is delivered to the SubscribePR callback for each SSE event
// that carries a valid compile.Summary payload.
type SummaryMessage struct {
	// Key is owner/repo#number@sha.
	Key string
	// Seq is the monotonic sequence number for the PR round.
	Seq int
	// Rendered is the human-readable summary text using severity symbols.
	Rendered string
	// Raw holds the full decoded Summary for callers that need structured access.
	Raw compile.Summary
}

// --- Client ---

// Client talks to a running Hub instance.
// All requests carry a Bearer token (ADR-0003).
type Client struct {
	hubURL string
	token  string
	http   *http.Client
}

// NewClient returns a Client pointed at hubURL with the given auth token.
func NewClient(hubURL, token string) *Client {
	return &Client{
		hubURL: strings.TrimRight(hubURL, "/"),
		token:  token,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// newRequest builds an authenticated request.
func (c *Client) newRequest(ctx context.Context, method, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.hubURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// GetPending calls GET /pending and returns all current pending items.
func (c *Client) GetPending(ctx context.Context) ([]PendingItem, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/pending")
	if err != nil {
		return nil, fmt.Errorf("get pending: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get pending: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("get pending: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Items []PendingItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("get pending: decode: %w", err)
	}
	return result.Items, nil
}

// AcquireRebaseLease requests a force-push lease for owner/repo#number from the Hub
// (ADR-0005). Returns a LeaseResult regardless of grant/deny (caller checks Granted).
// Rebase execution and heartbeat during rebase are Slice 6 — TODO(#6).
func (c *Client) AcquireRebaseLease(ctx context.Context, owner, repo string, number int) (LeaseResult, error) {
	path := fmt.Sprintf("/leases/%s/%s/%d", owner, repo, number)
	req, err := c.newRequest(ctx, http.MethodPost, path)
	if err != nil {
		return LeaseResult{}, fmt.Errorf("acquire rebase lease: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return LeaseResult{}, fmt.Errorf("acquire rebase lease: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 200 = granted, 409 = denied — both have a valid JSON body.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return LeaseResult{}, fmt.Errorf("acquire rebase lease: status %d: %s", resp.StatusCode, body)
	}

	var result LeaseResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return LeaseResult{}, fmt.Errorf("acquire rebase lease: decode: %w", err)
	}
	return result, nil
}

// RenewRebaseLease sends a heartbeat to PUT /leases/:owner/:repo/:number/heartbeat,
// which extends the lease TTL. holder must match the value returned by AcquireRebaseLease.
// Returns the updated LeaseResult on success.
func (c *Client) RenewRebaseLease(ctx context.Context, owner, repo string, number int, holder string) (LeaseResult, error) {
	path := fmt.Sprintf("/leases/%s/%s/%d/heartbeat", owner, repo, number)
	req, err := c.newRequest(ctx, http.MethodPut, path)
	if err != nil {
		return LeaseResult{}, fmt.Errorf("renew rebase lease: build request: %w", err)
	}
	req.Header.Set("X-Lease-Holder", holder)
	resp, err := c.http.Do(req)
	if err != nil {
		return LeaseResult{}, fmt.Errorf("renew rebase lease: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return LeaseResult{}, fmt.Errorf("renew rebase lease: status %d: %s", resp.StatusCode, body)
	}

	var result LeaseResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return LeaseResult{}, fmt.Errorf("renew rebase lease: decode: %w", err)
	}
	return result, nil
}

// ReleaseRebaseLease releases a force-push lease via DELETE /leases/:owner/:repo/:number.
// holder must match the value returned by AcquireRebaseLease.
func (c *Client) ReleaseRebaseLease(ctx context.Context, owner, repo string, number int, holder string) error {
	path := fmt.Sprintf("/leases/%s/%s/%d", owner, repo, number)
	req, err := c.newRequest(ctx, http.MethodDelete, path)
	if err != nil {
		return fmt.Errorf("release rebase lease: build request: %w", err)
	}
	req.Header.Set("X-Lease-Holder", holder)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("release rebase lease: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("release rebase lease: status %d: %s", resp.StatusCode, body)
	}
	return nil
}

// SubscribePR opens an SSE connection to GET /sse/:owner/:repo/:number and
// calls onMsg for each parsed compile.Summary it receives. It blocks until the
// context is canceled, the server closes the stream, or a fatal read error occurs.
// The rendered text in SummaryMessage uses severity.RenderPlain (symbol+label, no ANSI).
func (c *Client) SubscribePR(ctx context.Context, owner, repo string, number int, onMsg func(SummaryMessage)) error {
	path := fmt.Sprintf("/sse/%s/%s/%d", owner, repo, number)
	req, err := c.newRequest(ctx, http.MethodGet, path)
	if err != nil {
		return fmt.Errorf("subscribe pr: build request: %w", err)
	}

	// SSE connections use unbounded streaming — remove the timeout on this client.
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe pr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("subscribe pr: status %d: %s", resp.StatusCode, body)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}

		var s compile.Summary
		if err := json.Unmarshal([]byte(payload), &s); err != nil {
			// Non-fatal: skip unparseable SSE frames.
			continue
		}

		msg := SummaryMessage{
			Key:      s.Key,
			Seq:      s.Seq,
			Rendered: renderSummary(s),
			Raw:      s,
		}
		onMsg(msg)
	}

	if err := scanner.Err(); err != nil {
		// Context cancellation manifests as a read error on the body.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("subscribe pr: read: %w", err)
	}
	return nil
}

// renderSummary builds a human-readable summary using severity symbols (ADR-0004).
// It reuses internal/severity's renderer — never duplicates or modifies it.
func renderSummary(s compile.Summary) string {
	if s.Text != "" {
		return annotateWithSeveritySymbols(s)
	}
	return s.Key
}

// annotateWithSeveritySymbols walks the compile.Summary and prepends
// severity symbol+label prefixes to each item body using severity.RenderPlain.
// The summary text from the Hub already contains the plain text; we enrich it
// by building a block per group that shows the symbol beside each item.
func annotateWithSeveritySymbols(s compile.Summary) string {
	var b strings.Builder
	b.WriteString(s.Text)
	if len(s.Groups) == 0 {
		return b.String()
	}
	b.WriteString("\n")
	for _, g := range s.Groups {
		for _, item := range g.Items {
			lvl := parseSeverityLabel(item.Severity)
			symbol := severity.RenderPlain(lvl)
			b.WriteString("\n  ")
			b.WriteString(symbol)
			if item.Body != "" {
				b.WriteString(": ")
				b.WriteString(item.Body)
			}
		}
	}
	return b.String()
}

// parseSeverityLabel converts a severity string from the compile layer
// ("critical" / "major" / "minor" / "") back to a severity.Level.
func parseSeverityLabel(s string) severity.Level {
	switch strings.ToLower(s) {
	case "critical":
		return severity.Critical
	case "major":
		return severity.Major
	default:
		return severity.Minor
	}
}
