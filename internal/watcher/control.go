// Package watcher — control-plane SSE consumer for auth-v2 Phase 3.5.
//
// ControlLoop holds a long-running GET /sse/me/control connection against the
// hub. The hub publishes `pr_opened` / `pr_closed` / `installation_added`
// events keyed on the authenticated github_user_id; the loop dispatches each
// to a SubscriberPool so the watcher auto-subscribes to PRs the user just
// raised without the agent ever calling `subscribe_pr` first.
//
// Reconnection: a transport error or server close triggers a jittered
// exponential backoff (min 1s, max 30s) before the next dial. After 5 min of
// stability the backoff resets to min, so a flaky network early in a session
// doesn't permanently lengthen later reconnect attempts. Failure-noise
// thresholds (info → warn → error) match the plan's ladder.
package watcher

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// ControlLoopOptions configures ControlLoop. Defaults are filled by
// ensureDefaults() — tests inject fast backoff so reconnect coverage runs in
// a deterministic, sub-second budget.
type ControlLoopOptions struct {
	HubURL string

	// TokenFn returns the currently authenticated user-bound token. The
	// loop re-evaluates on every dial, so a fresh login (without process
	// restart) picks up the new credentials at the next reconnect. Return
	// ok=false to signal "not logged in" — the loop exits cleanly.
	TokenFn func() (token string, ok bool)

	// Pool receives Subscribe / Unsubscribe per pr_opened / pr_closed event.
	Pool ControlPool

	// OnInstallationAdded is invoked for `installation_added` frames so the
	// caller can invalidate a cached /me response. Optional.
	OnInstallationAdded func(installationID, org string)

	// BackoffMin / BackoffMax bound the reconnect timer. Defaults: 1s / 30s.
	BackoffMin time.Duration
	BackoffMax time.Duration

	// StableReset is how long a connection must stay up before the backoff
	// resets to BackoffMin. Default: 5 min.
	StableReset time.Duration

	// HTTPClient is the dialer for /sse/me/control. nil → an unbounded
	// timeout client (SSE streams must not be timeout-capped).
	HTTPClient *http.Client
}

func (o *ControlLoopOptions) ensureDefaults() {
	if o.BackoffMin == 0 {
		o.BackoffMin = time.Second
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = 30 * time.Second
	}
	if o.StableReset == 0 {
		o.StableReset = 5 * time.Minute
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{} // no timeout — SSE is unbounded
	}
}

// ControlPool is the subset of *Pool the control loop drives. Mockable.
type ControlPool interface {
	Subscribe(ctx context.Context, owner, repo string, number int, token string) bool
	Unsubscribe(owner, repo string, number int)
}

// ControlLoop runs until ctx is done, holding /sse/me/control open and
// dispatching every frame to the pool. Reconnects on transport error /
// server close with jittered exponential backoff.
//
// Returns when ctx.Done() fires — never on its own. A ctx without cancel
// signal would run forever; the caller MUST own cancellation.
func ControlLoop(ctx context.Context, opts ControlLoopOptions) {
	opts.ensureDefaults()
	if opts.HubURL == "" || opts.TokenFn == nil || opts.Pool == nil {
		log.Printf("control loop: missing required option; exiting")
		return
	}

	backoff := opts.BackoffMin
	failures := 0

	for ctx.Err() == nil {
		token, ok := opts.TokenFn()
		if !ok || token == "" {
			log.Printf("control loop: no user-bound token available; exiting")
			return
		}

		start := time.Now()
		err := connectOnce(ctx, opts, token)
		if ctx.Err() != nil {
			return
		}
		stability := time.Since(start)
		if stability >= opts.StableReset {
			backoff = opts.BackoffMin
			failures = 0
		} else {
			failures++
			logReconnect(failures, err)
		}

		sleep := jitter(backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		backoff *= 2
		if backoff > opts.BackoffMax {
			backoff = opts.BackoffMax
		}
	}
}

// connectOnce dials /sse/me/control, parses every SSE frame until the stream
// closes or ctx is canceled. Returns the underlying error (nil on clean
// server-close, ctx.Err() on cancel, transport error otherwise).
func connectOnce(ctx context.Context, opts ControlLoopOptions, token string) error {
	url := strings.TrimRight(opts.HubURL, "/") + "/sse/me/control"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}

	parseFrames(ctx, resp.Body, opts, token)
	return nil
}

// parseFrames reads one SSE frame at a time from r, dispatching each
// recognized event to the pool / installation callback. Unknown event types
// are ignored (forward compatibility — adding a new event must not crash
// older watchers).
func parseFrames(ctx context.Context, r io.Reader, opts ControlLoopOptions, token string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	var eventName, dataLine string
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if eventName != "" && dataLine != "" {
				dispatch(ctx, opts, token, eventName, dataLine)
			}
			eventName, dataLine = "", ""
		}
	}
}

// prOpenedFrame is the wire payload for `pr_opened`.
type prOpenedFrame struct {
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	HeadSHA     string `json:"head_sha"`
	AuthorLogin string `json:"author_login"`
}

// prClosedFrame is the wire payload for `pr_closed`.
type prClosedFrame struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// installationAddedFrame is the wire payload for `installation_added`.
type installationAddedFrame struct {
	InstallationID string `json:"installation_id"`
	Org            string `json:"org"`
}

func dispatch(ctx context.Context, opts ControlLoopOptions, token, name, data string) {
	switch name {
	case "pr_opened":
		var f prOpenedFrame
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			log.Printf("control loop: parse pr_opened: %v", err)
			return
		}
		if opts.Pool.Subscribe(ctx, f.Owner, f.Repo, f.Number, token) {
			log.Printf("auto-subscribed %s/%s#%d via control stream",
				f.Owner, f.Repo, f.Number)
		}
	case "pr_closed":
		var f prClosedFrame
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			log.Printf("control loop: parse pr_closed: %v", err)
			return
		}
		opts.Pool.Unsubscribe(f.Owner, f.Repo, f.Number)
	case "installation_added":
		if opts.OnInstallationAdded == nil {
			return
		}
		var f installationAddedFrame
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			log.Printf("control loop: parse installation_added: %v", err)
			return
		}
		opts.OnInstallationAdded(f.InstallationID, f.Org)
	case "ping":
		// keepalive — drop silently
	}
}

// jitter spreads reconnect attempts by ±20% so a hub restart doesn't have
// every watcher reconnect at exactly the same instant. Output is bounded
// below by 100ms so a small base doesn't collapse to zero on bad luck.
func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 100 * time.Millisecond
	}
	delta := time.Duration(rand.Int63n(int64(base) / 5)) //nolint:gosec // jitter, not security
	if rand.Intn(2) == 0 {                               //nolint:gosec
		return base - delta
	}
	return base + delta
}

func logReconnect(failures int, err error) {
	switch {
	case failures == 1:
		log.Printf("control loop: disconnected (%v); reconnecting", err)
	case failures < 3:
		log.Printf("control loop: reconnect attempt %d (%v)", failures, err)
	case failures < 10:
		log.Printf("control loop: WARN reconnect attempt %d (%v)", failures, err)
	default:
		log.Printf("control loop: ERROR reconnect attempt %d (%v)", failures, err)
	}
}
