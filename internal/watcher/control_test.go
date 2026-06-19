// Auth-v2 Phase 3.5 (issue #60): control-plane SSE consumer tests.
package watcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePool records Subscribe / Unsubscribe calls.
type fakePool struct {
	mu          sync.Mutex
	subscribes  []string // "owner/repo#number"
	unsubs      []string
	subscribed  map[string]struct{}
	returnFalse bool
}

func newFakePool() *fakePool {
	return &fakePool{subscribed: make(map[string]struct{})}
}

func (p *fakePool) Subscribe(_ context.Context, owner, repo string, number int, _ string) bool {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.returnFalse {
		return false
	}
	if _, dup := p.subscribed[key]; dup {
		return false
	}
	p.subscribed[key] = struct{}{}
	p.subscribes = append(p.subscribes, key)
	return true
}

func (p *fakePool) Unsubscribe(owner, repo string, number int) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.subscribed, key)
	p.unsubs = append(p.unsubs, key)
}

func (p *fakePool) calls() (subs, unsubs []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	subs = append(subs, p.subscribes...)
	unsubs = append(unsubs, p.unsubs...)
	return
}

// TestControlLoop_AutoSubscribesOnPROpen — a fake hub streams pr_opened then
// pr_closed; the loop calls Subscribe once and Unsubscribe once.
func TestControlLoop_AutoSubscribesOnPROpen(t *testing.T) {
	pool := newFakePool()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: pr_opened\ndata: {\"owner\":\"o\",\"repo\":\"r\",\"number\":42,\"head_sha\":\"abc\",\"author_login\":\"a\"}\n\n")
		fl.Flush()
		_, _ = fmt.Fprintf(w, "event: pr_closed\ndata: {\"owner\":\"o\",\"repo\":\"r\",\"number\":42}\n\n")
		fl.Flush()
		// Sleep briefly so the client parses both frames before EOF.
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ControlLoop(ctx, ControlLoopOptions{
		HubURL:      srv.URL,
		TokenFn:     func() (string, bool) { return "tok", true },
		Pool:        pool,
		BackoffMin:  10 * time.Millisecond,
		BackoffMax:  20 * time.Millisecond,
		StableReset: time.Hour, // never reset
	})

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		subs, unsubs := pool.calls()
		if len(subs) >= 1 && len(unsubs) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	subs, unsubs := pool.calls()
	if len(subs) < 1 || subs[0] != "o/r#42" {
		t.Fatalf("subscribes = %v, want o/r#42", subs)
	}
	if len(unsubs) < 1 || unsubs[0] != "o/r#42" {
		t.Fatalf("unsubscribes = %v, want o/r#42", unsubs)
	}
}

// TestControlLoop_ReconnectsOnDrop — the fake server closes after the first
// pr_opened; the loop must reconnect and receive a second pr_opened.
func TestControlLoop_ReconnectsOnDrop(t *testing.T) {
	pool := newFakePool()
	var dialCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := dialCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// First dial streams PR 1, then closes (returns).
		// Second dial streams PR 2, then closes.
		_, _ = fmt.Fprintf(w, "event: pr_opened\ndata: {\"owner\":\"o\",\"repo\":\"r\",\"number\":%d}\n\n", n)
		fl.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ControlLoop(ctx, ControlLoopOptions{
		HubURL:      srv.URL,
		TokenFn:     func() (string, bool) { return "tok", true },
		Pool:        pool,
		BackoffMin:  10 * time.Millisecond,
		BackoffMax:  20 * time.Millisecond,
		StableReset: time.Hour,
	})

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && dialCount.Load() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := dialCount.Load(); got < 2 {
		t.Fatalf("dial count = %d, want >=2 (reconnect did not fire)", got)
	}
	subs, _ := pool.calls()
	if len(subs) < 2 {
		t.Fatalf("subscribes = %v, want at least 2 (one per dial)", subs)
	}
}

// TestControlLoop_ExitsOnMissingToken — TokenFn returning ok=false MUST cause
// a clean exit, not a hot reconnect loop.
func TestControlLoop_ExitsOnMissingToken(t *testing.T) {
	pool := newFakePool()
	done := make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		ControlLoop(ctx, ControlLoopOptions{
			HubURL:      "http://example.invalid",
			TokenFn:     func() (string, bool) { return "", false },
			Pool:        pool,
			BackoffMin:  10 * time.Millisecond,
			BackoffMax:  20 * time.Millisecond,
			StableReset: time.Hour,
		})
		close(done)
	}()

	select {
	case <-done:
		// success — loop returned because TokenFn signaled "not logged in"
	case <-time.After(time.Second):
		t.Fatal("ControlLoop did not exit on TokenFn ok=false")
	}
}

// TestControlLoop_InstallationAddedCallback — installation_added frames fire
// the OnInstallationAdded hook.
func TestControlLoop_InstallationAddedCallback(t *testing.T) {
	pool := newFakePool()
	var seen atomic.Int32
	var gotID, gotOrg string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: installation_added\ndata: {\"installation_id\":\"42\",\"org\":\"acme\"}\n\n")
		fl.Flush()
		time.Sleep(150 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ControlLoop(ctx, ControlLoopOptions{
		HubURL:     srv.URL,
		TokenFn:    func() (string, bool) { return "tok", true },
		Pool:       pool,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
		OnInstallationAdded: func(id, org string) {
			mu.Lock()
			gotID, gotOrg = id, org
			mu.Unlock()
			seen.Add(1)
		},
		StableReset: time.Hour,
	})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && seen.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if seen.Load() == 0 {
		t.Fatal("OnInstallationAdded never fired")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotID != "42" || gotOrg != "acme" {
		t.Errorf("got (%q,%q), want (42,acme)", gotID, gotOrg)
	}
}

// TestPool_SubscribeDedups — the subscriber pool de-dups on key. Two
// Subscribe calls for the same key result in one active entry.
func TestPool_SubscribeDedups(t *testing.T) {
	p := NewPool(blockingPumper{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !p.Subscribe(ctx, "o", "r", 1, "tok") {
		t.Fatal("first Subscribe should return true")
	}
	if p.Subscribe(ctx, "o", "r", 1, "tok") {
		t.Fatal("duplicate Subscribe should return false")
	}
	if !p.Has("o", "r", 1) {
		t.Fatal("Has should be true after Subscribe")
	}
	if p.Count() != 1 {
		t.Fatalf("Count = %d, want 1", p.Count())
	}

	p.Unsubscribe("o", "r", 1)
	if p.Has("o", "r", 1) {
		t.Fatal("Has should be false after Unsubscribe")
	}
}

// blockingPumper blocks until ctx is canceled — simulates a long-lived
// SSE subscription for pool tests.
type blockingPumper struct{}

func (blockingPumper) Pump(ctx context.Context, _, _ string, _ int, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}
