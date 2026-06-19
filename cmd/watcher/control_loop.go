// Auth-v2 Phase 3.5 (issue #60) — control-stream wiring for the watcher.
//
// startControlLoop reads the credentials file, picks the first user-bound
// token, and spawns ControlLoop in a background goroutine. The loop re-reads
// credentials on every reconnect dial, so a fresh `login` (without restart)
// picks up the new token at the next reconnect — and a `logout` (TokenFn
// returns ok=false) drops the loop cleanly.
package main

import (
	"context"
	"log"

	"github.com/ravencloak-org/caw/internal/watcher"
)

// noopPumper satisfies watcher.Pumper without opening a per-PR SSE
// connection. Auto-subscriptions stay in the pool for bookkeeping; the hub
// sees no per-PR subscriber and falls back to its pending store, which the
// agent polls via the existing get_pending tool. A later phase swaps this
// for a real pump + local summary buffer.
type noopPumper struct{}

func (noopPumper) Pump(ctx context.Context, _, _ string, _ int, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// startControlLoop launches ControlLoop in a background goroutine. It is a
// no-op when no credentials file exists — the user hasn't logged in yet, and
// the control stream is only useful once they have.
func startControlLoop(ctx context.Context, hubURL string) {
	credsPath, err := watcher.DefaultCredentialsPath()
	if err != nil {
		log.Printf("control loop: resolve credentials path: %v; skipping", err)
		return
	}

	pool := watcher.NewPool(noopPumper{})

	tokenFn := func() (string, bool) {
		creds, ok, err := watcher.LoadCredentials(credsPath)
		if err != nil || !ok {
			return "", false
		}
		if creds.HubURL != "" && creds.HubURL != hubURL {
			return "", false
		}
		tok, ok := creds.FirstToken()
		return tok, ok && creds.GitHubUserID != 0
	}

	if tok, ok := tokenFn(); !ok || tok == "" {
		log.Printf("control loop: no user-bound credentials yet; auto-subscribe disabled (run `login`)")
		return
	}

	go watcher.ControlLoop(ctx, watcher.ControlLoopOptions{
		HubURL:  hubURL,
		TokenFn: tokenFn,
		Pool:    pool,
	})
}
