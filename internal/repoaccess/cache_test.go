package repoaccess

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChecker is a controllable Checker for cache tests. Tests script
// `next` to choose the result for the next HasReadAccess call; calls
// counts on `n` for assertion of cache-hit vs cache-miss.
type fakeChecker struct {
	mu     sync.Mutex
	allow  bool
	err    error
	delay  time.Duration
	calls  int64
	gateCh chan struct{} // when non-nil, HasReadAccess blocks on it
}

func (f *fakeChecker) HasReadAccess(ctx context.Context, _, _, _, _ string) (bool, error) {
	atomic.AddInt64(&f.calls, 1)
	f.mu.Lock()
	gate := f.gateCh
	delay := f.delay
	allow := f.allow
	err := f.err
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return allow, err
}

// fakeClock returns a programmable clock backed by a *time.Time.
func fakeClock(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}

const (
	tInst  = "inst-1"
	tUser  = int64(42)
	tLogin = "alice"
	tOwner = "octocorp"
	tRepo  = "widgets"
)

func newTestCache(t *testing.T, fc *fakeChecker, now *time.Time) *Cache {
	t.Helper()
	return NewCache(fc, Options{
		PositiveTTL: 5 * time.Minute,
		NegativeTTL: 60 * time.Second,
		StaleGrace:  30 * time.Minute,
		SweepEvery:  1 * time.Hour, // disabled effectively for tests
		NowFn:       fakeClock(now),
	})
}

// TestCache_PositiveHitServesFromCache: two Lookups with checker.allow=true
// result in one checker call (the second is a hit).
func TestCache_PositiveHitServesFromCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)

	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if err != nil || !allowed || src != SourceMissAllow {
		t.Fatalf("first lookup: allowed=%v src=%q err=%v want true/%q", allowed, src, err, SourceMissAllow)
	}

	allowed, src, err = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if err != nil || !allowed || src != SourceHit {
		t.Fatalf("second lookup: allowed=%v src=%q err=%v want true/%q", allowed, src, err, SourceHit)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 1 {
		t.Errorf("checker called %d times, want 1", got)
	}
}

// TestCache_NegativeHitServesFromCacheWithin60s: a deny is cached for 60s.
func TestCache_NegativeHitServesFromCacheWithin60s(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: false} // simulates 404 → deny without error
	c := newTestCache(t, fc, &now)

	if allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo); err != nil || allowed || src != SourceMissDeny {
		t.Fatalf("first: allowed=%v src=%q err=%v want false/%q", allowed, src, err, SourceMissDeny)
	}
	if allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo); err != nil || allowed || src != SourceNegativeHit {
		t.Fatalf("second: allowed=%v src=%q err=%v want false/%q", allowed, src, err, SourceNegativeHit)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 1 {
		t.Errorf("checker called %d times, want 1", got)
	}
}

// TestCache_PositiveTTLExpires: after 5 min the positive entry is re-fetched.
func TestCache_PositiveTTLExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)

	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	now = now.Add(5*time.Minute + time.Second)
	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if err != nil || !allowed || src != SourceMissAllow {
		t.Fatalf("post-TTL: allowed=%v src=%q err=%v want true/%q", allowed, src, err, SourceMissAllow)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 2 {
		t.Errorf("checker called %d times, want 2", got)
	}
}

// TestCache_NegativeTTLExpires: after 60s a negative entry is re-fetched.
func TestCache_NegativeTTLExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: false}
	c := newTestCache(t, fc, &now)

	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	now = now.Add(61 * time.Second)
	if allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo); err != nil || allowed || src != SourceMissDeny {
		t.Fatalf("post-TTL: allowed=%v src=%q err=%v want false/%q", allowed, src, err, SourceMissDeny)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 2 {
		t.Errorf("checker called %d times, want 2", got)
	}
}

// TestCache_StaleAllowOnTransientFailure: positive entry past TTL but within
// 30 min, GitHub returns ErrUnavailable → stale-allow.
func TestCache_StaleAllowOnTransientFailure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)
	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)

	// Advance past positive TTL (5m) but well within the 30m grace.
	now = now.Add(10 * time.Minute)
	fc.mu.Lock()
	fc.err = fmt.Errorf("%w: 503", ErrUnavailable)
	fc.mu.Unlock()

	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if err != nil || !allowed || src != SourceStale {
		t.Fatalf("stale-allow: allowed=%v src=%q err=%v want true/%q nil", allowed, src, err, SourceStale)
	}
}

// TestCache_FailClosedOnUnavailableWithoutPriorCache: cold miss + 5xx →
// (false, unavailable, ErrUnavailable).
func TestCache_FailClosedOnUnavailableWithoutPriorCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{err: fmt.Errorf("%w: 503", ErrUnavailable)}
	c := newTestCache(t, fc, &now)

	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceUnavailable || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cold-5xx: allowed=%v src=%q err=%v want false/%q ErrUnavailable", allowed, src, err, SourceUnavailable)
	}
}

// TestCache_FailClosedAfterNegativeEntry: stale-allow MUST NOT widen a prior
// deny — a negative entry past its TTL + 5xx still fails closed.
func TestCache_FailClosedAfterNegativeEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: false}
	c := newTestCache(t, fc, &now)
	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)

	now = now.Add(2 * time.Minute) // past negTTL but still well within posTTL-equivalent
	fc.mu.Lock()
	fc.err = fmt.Errorf("%w: 503", ErrUnavailable)
	fc.mu.Unlock()
	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceUnavailable || err == nil {
		t.Fatalf("stale-deny-then-5xx: allowed=%v src=%q err=%v want false/%q with err", allowed, src, err, SourceUnavailable)
	}
}

// TestCache_StaleAllowExpiresAtGrace: past the 30-min grace, a positive entry
// no longer rescues a 5xx.
func TestCache_StaleAllowExpiresAtGrace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)
	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)

	now = now.Add(31 * time.Minute)
	fc.mu.Lock()
	fc.err = fmt.Errorf("%w: 503", ErrUnavailable)
	fc.mu.Unlock()
	allowed, src, _ := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceUnavailable {
		t.Fatalf("post-grace: allowed=%v src=%q want false/%q", allowed, src, SourceUnavailable)
	}
}

// TestCache_ConfigErrorPropagates: 403 from GitHub surfaces as ErrConfigError.
func TestCache_ConfigErrorPropagates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{err: fmt.Errorf("%w: 403", ErrConfigError)}
	c := newTestCache(t, fc, &now)
	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceConfigError || err == nil || !errors.Is(err, ErrConfigError) {
		t.Fatalf("config-err: allowed=%v src=%q err=%v want false/%q ErrConfigError", allowed, src, err, SourceConfigError)
	}
}

// TestCache_DifferentUsersDoNotCollide: a positive entry for user A must
// not satisfy a lookup for user B with the same (inst, owner, repo).
func TestCache_DifferentUsersDoNotCollide(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)

	if _, _, err := c.Lookup(context.Background(), tInst, 100, "alice", tOwner, tRepo); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, _, err := c.Lookup(context.Background(), tInst, 200, "bob", tOwner, tRepo); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 2 {
		t.Errorf("checker called %d times, want 2 (two distinct users)", got)
	}
}

// TestCache_LegacyUserIDBypass: a userID of 0 is the legacy sentinel; Lookup
// returns SourceLegacy without consulting the checker.
func TestCache_LegacyUserIDBypass(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)
	allowed, src, err := c.Lookup(context.Background(), tInst, 0, "", tOwner, tRepo)
	if allowed || src != SourceLegacy || err != nil {
		t.Fatalf("legacy: allowed=%v src=%q err=%v want false/%q nil", allowed, src, err, SourceLegacy)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 0 {
		t.Errorf("checker called %d times for legacy lookup, want 0", got)
	}
}

// TestCache_NilChecker_UnavailableOnMiss: a Cache built without a Checker
// fails closed (this guards the server-test path where a nil checker is
// wired because all tokens are legacy).
func TestCache_NilChecker_UnavailableOnMiss(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := NewCache(nil, Options{NowFn: fakeClock(&now)})
	allowed, src, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceUnavailable || err == nil {
		t.Fatalf("nil-checker: allowed=%v src=%q err=%v want false/%q with err", allowed, src, err, SourceUnavailable)
	}
}

// TestCache_MalformedKeyFailsClosed: empty owner/repo/userLogin/instID must
// not silently allow; defensive check returns unavailable.
func TestCache_MalformedKeyFailsClosed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)
	allowed, src, err := c.Lookup(context.Background(), "", tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceUnavailable || err == nil {
		t.Fatalf("malformed: allowed=%v src=%q err=%v want false/%q with err", allowed, src, err, SourceUnavailable)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 0 {
		t.Errorf("checker called %d times for malformed key, want 0", got)
	}
}

// TestCache_FlushRepoDropsEntry: a flush of the (inst, owner/repo) tuple
// forces the next lookup to re-fetch.
func TestCache_FlushRepoDropsEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)

	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if c.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after seed", c.Len())
	}

	c.FlushRepo(tInst, tOwner+"/"+tRepo)
	if c.Len() != 0 {
		t.Fatalf("Len() = %d, want 0 after FlushRepo", c.Len())
	}

	_, src, _ := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	if src != SourceMissAllow {
		t.Fatalf("post-flush src = %q, want %q (re-fetched)", src, SourceMissAllow)
	}
}

// TestCache_FlushRepoLeavesOtherReposIntact: only the targeted (inst, repo)
// is evicted; the same installation's other repos survive.
func TestCache_FlushRepoLeavesOtherReposIntact(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)
	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, "alpha")
	_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, "beta")
	c.FlushRepo(tInst, tOwner+"/alpha")
	if _, src, _ := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, "beta"); src != SourceHit {
		t.Errorf("untouched repo: src = %q, want %q", src, SourceHit)
	}
}

// TestCache_FlushInstallationDropsAllEntries: a flush of the whole
// installation wipes every user/repo cached under it.
func TestCache_FlushInstallationDropsAllEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)

	_, _, _ = c.Lookup(context.Background(), "inst-A", 1, "u1", "o", "r1")
	_, _, _ = c.Lookup(context.Background(), "inst-A", 2, "u2", "o", "r2")
	_, _, _ = c.Lookup(context.Background(), "inst-B", 1, "u1", "o", "r1")

	c.FlushInstallation("inst-A")
	if c.Len() != 1 {
		t.Errorf("after FlushInstallation(A): Len() = %d, want 1 (inst-B survives)", c.Len())
	}
	// inst-B entry must still be a hit.
	if _, src, _ := c.Lookup(context.Background(), "inst-B", 1, "u1", "o", "r1"); src != SourceHit {
		t.Errorf("inst-B post-flush: src = %q, want %q", src, SourceHit)
	}
}

// TestCache_SingleFlightCollapsesConcurrentMisses: N concurrent Lookups on
// a cold key should fire exactly one HasReadAccess call.
func TestCache_SingleFlightCollapsesConcurrentMisses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := make(chan struct{})
	fc := &fakeChecker{allow: true, gateCh: gate}
	c := newTestCache(t, fc, &now)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			allowed, _, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
			if err != nil {
				errs <- err
				return
			}
			if !allowed {
				errs <- fmt.Errorf("unexpected deny")
			}
		}()
	}
	// Give all goroutines a moment to enter Lookup and queue on inflight.
	time.Sleep(20 * time.Millisecond)
	close(gate) // release the one in-flight fetch
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("goroutine error: %v", err)
	}
	if got := atomic.LoadInt64(&fc.calls); got != 1 {
		t.Errorf("single-flight: checker called %d times across %d concurrent lookups, want 1", got, n)
	}
}

// TestCache_ContextCancelWhileInflight: a goroutine waiting on the inflight
// channel respects ctx cancellation.
func TestCache_ContextCancelWhileInflight(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := make(chan struct{})
	fc := &fakeChecker{allow: true, gateCh: gate}
	c := newTestCache(t, fc, &now)

	first := make(chan struct{})
	go func() {
		_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
		close(first)
	}()
	// Let the first goroutine register inflight.
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled
	allowed, src, err := c.Lookup(ctx, tInst, tUser, tLogin, tOwner, tRepo)
	if allowed || src != SourceUnavailable || err == nil {
		t.Fatalf("canceled wait: allowed=%v src=%q err=%v want false/%q with err", allowed, src, err, SourceUnavailable)
	}
	close(gate)
	<-first
}

// TestCache_SweepEvictsExpiredEntries: sweepOnce drops past-grace positives
// and past-TTL negatives.
func TestCache_SweepEvictsExpiredEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := newTestCache(t, fc, &now)
	_, _, _ = c.Lookup(context.Background(), tInst, 1, "u", tOwner, "alpha")
	fc.mu.Lock()
	fc.allow = false
	fc.mu.Unlock()
	_, _, _ = c.Lookup(context.Background(), tInst, 2, "u", tOwner, "beta")
	if c.Len() != 2 {
		t.Fatalf("seed: Len()=%d want 2", c.Len())
	}

	// Past 30-min grace evicts the positive; past 60s TTL evicts the negative.
	c.sweepOnce(now.Add(31 * time.Minute))
	if c.Len() != 0 {
		t.Errorf("post-sweep: Len()=%d want 0", c.Len())
	}
}

// TestCache_StartStopsOnContextCancel: Start launches a goroutine that
// returns when ctx is canceled (sanity / leak-prevention smoke).
func TestCache_StartStopsOnContextCancel(t *testing.T) {
	c := NewCache(&fakeChecker{}, Options{SweepEvery: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	time.Sleep(25 * time.Millisecond) // let sweeper run a couple times
	cancel()
	time.Sleep(20 * time.Millisecond) // give goroutine time to exit
	// Assertion is implicit: the test exits cleanly (no goroutine leak, no
	// panic on the channel close path). Logging t.Name keeps revive happy
	// about the otherwise-unused t.
	t.Logf("%s: sweeper exited after ctx cancel", t.Name())
}

// BenchmarkCacheHit times a positive cache hit. The acceptance gate asks
// for ≤ 1 µs / op on the hub developer's laptop; we keep this here as a
// sanity check and a perf-regression tripwire (not enforced).
func BenchmarkCacheHit(b *testing.B) {
	now := time.Unix(1_700_000_000, 0)
	fc := &fakeChecker{allow: true}
	c := NewCache(fc, Options{NowFn: fakeClock(&now)})
	if _, _, err := c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo); err != nil {
		b.Fatalf("seed: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = c.Lookup(context.Background(), tInst, tUser, tLogin, tOwner, tRepo)
	}
}
