// Package watcher — per-PR subscription bookkeeping for auth-v2 Phase 3.5.
//
// The SubscriberPool is the in-process registry of PRs the watcher is
// auto-subscribed to via the control stream. It's a thin bookkeeping layer
// the ControlLoop drives — Subscribe on `pr_opened`, Unsubscribe on
// `pr_closed` — that de-dups on `owner/repo#number` so a manual subscribe_pr
// invocation followed by a webhook event doesn't double-attach.
//
// The actual SSE streaming for each PR happens via the Pumper interface; the
// pool calls Pump once per Subscribe and tracks the cancel func so
// Unsubscribe releases the per-key goroutine deterministically.
package watcher

import (
	"context"
	"fmt"
	"sync"
)

// Pumper opens an SSE subscription for one PR. The pool calls Pump in a
// goroutine; cancellation of ctx is the unsubscribe signal. token is the
// authenticated user-bound token to present on the request.
//
// The production implementation lives in DefaultPumper (calls into the same
// Client.SubscribePR machinery the manual subscribe_pr tool uses); tests
// inject a fake to assert Subscribe / Unsubscribe bookkeeping without ever
// touching the network.
type Pumper interface {
	Pump(ctx context.Context, owner, repo string, number int, token string) error
}

// Pool is the per-PR subscriber registry. Methods are safe to call from any
// goroutine; the internal map is guarded by a mutex.
type Pool struct {
	mu     sync.Mutex
	pump   Pumper
	active map[string]context.CancelFunc
}

// NewPool returns an empty Pool that drives pump for each new subscription.
// pump MUST be non-nil — a nil pumper would silently swallow every Subscribe.
func NewPool(pump Pumper) *Pool {
	return &Pool{pump: pump, active: make(map[string]context.CancelFunc)}
}

// Key formats the canonical pool key "owner/repo#number". Callers that have
// the three pieces (the control stream's pr_opened payload) use this helper
// so the format stays consistent with the per-PR Hub key.
func Key(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// Subscribe registers key and starts a per-key SSE pump goroutine.
// Idempotent: a second call with the same key is a no-op (returns false).
// Returns true when this call actually started a new goroutine.
//
// token is held by the pumper goroutine, not by the pool; revoking the token
// server-side will surface as an error from Pump and the goroutine exits.
func (p *Pool) Subscribe(parent context.Context, owner, repo string, number int, token string) bool {
	key := Key(owner, repo, number)
	p.mu.Lock()
	if _, present := p.active[key]; present {
		p.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(parent)
	p.active[key] = cancel
	p.mu.Unlock()

	go func() {
		_ = p.pump.Pump(ctx, owner, repo, number, token)
		// Goroutine exit (transport error, cancel, server close) drops the
		// entry so a follow-up Subscribe for the same key re-attaches.
		p.mu.Lock()
		if c, present := p.active[key]; present && fmt.Sprintf("%p", c) == fmt.Sprintf("%p", cancel) {
			delete(p.active, key)
		}
		p.mu.Unlock()
	}()
	return true
}

// Unsubscribe cancels the per-key pump goroutine. No-op when key is unknown.
func (p *Pool) Unsubscribe(owner, repo string, number int) {
	key := Key(owner, repo, number)
	p.mu.Lock()
	cancel := p.active[key]
	delete(p.active, key)
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Has reports whether key is currently subscribed. Used by ControlLoop tests
// and by future "list active auto-subscriptions" surfaces.
func (p *Pool) Has(owner, repo string, number int) bool {
	key := Key(owner, repo, number)
	p.mu.Lock()
	defer p.mu.Unlock()
	_, present := p.active[key]
	return present
}

// Count returns the number of live subscriptions; useful for diagnostics
// and tests that need to wait for all goroutines to drain.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}
