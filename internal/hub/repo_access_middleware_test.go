package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/repoaccess"
)

func init() { gin.SetMode(gin.TestMode) }

// stubChecker is a deterministic Checker for middleware tests.
type stubChecker struct {
	mu     sync.Mutex
	allow  bool
	err    error
	calls  int
	lastUL string // last userLogin observed
}

func (s *stubChecker) HasReadAccess(_ context.Context, _, userLogin, _, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastUL = userLogin
	return s.allow, s.err
}

// rapHarness wires a minimal gin engine: injectCtx populates the auth context
// keys the way auth.Required does, then RequireRepoAccess runs against the
// supplied cache, then a sentinel handler reports "ok". Uses the Phase 5
// default (AllowLegacyTokens=false). Tests exercising the operator escape
// hatch use rapHarnessWithOpts directly.
func rapHarness(t *testing.T, cache *repoaccess.Cache, userID int64, login string) *gin.Engine {
	return rapHarnessWithOpts(t, cache, userID, login, RequireRepoAccessOptions{})
}

// rapHarnessWithOpts is rapHarness with an explicit Options struct, used by
// the Phase 5 cutover tests to verify both default-reject and escape-hatch
// behavior on the same plumbing.
func rapHarnessWithOpts(t *testing.T, cache *repoaccess.Cache, userID int64, login string, opts RequireRepoAccessOptions) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextInstallationID, "inst-1")
		c.Set(auth.ContextTokenID, "tok-abc")
		c.Set(auth.ContextGitHubUserID, userID)
		c.Set(auth.ContextGitHubUserLogin, login)
		c.Next()
	})
	r.GET("/sse/:owner/:repo/:number", RequireRepoAccess(cache, opts), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	return r
}

func newFakeClock(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}

// TestRequireRepoAccess_LegacyTokenRejectedWith400: Phase 5 cutover — a
// userID of 0 (legacy sentinel) is rejected with 400 + JSON actionable body
// pointing the MCP at the login flow. No GitHub call, no Deprecation header.
func TestRequireRepoAccess_LegacyTokenRejectedWith400(t *testing.T) {
	sc := &stubChecker{} // must not be called
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 0, "")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/octocorp/widgets/1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if got := w.Header().Get("Deprecation"); got != "" {
		t.Errorf("Deprecation = %q on reject path, want empty", got)
	}
	body := w.Body.String()
	for _, want := range []string{
		"user-bound token required",
		"login",
		"/auth/start",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
	if sc.calls != 0 {
		t.Errorf("checker called %d times for rejected legacy, want 0", sc.calls)
	}
}

// TestRequireRepoAccess_LegacyTokenAllowedByOperatorEscapeHatch: the
// CAW_ALLOW_LEGACY_TOKENS=1 escape hatch (AllowLegacyTokens=true) restores
// the Phase 2 bypass behavior — 200 + Deprecation: legacy-token, no
// checker call. One more release of migration headroom; documented as
// temporary.
func TestRequireRepoAccess_LegacyTokenAllowedByOperatorEscapeHatch(t *testing.T) {
	sc := &stubChecker{} // must not be called even with the escape hatch on
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarnessWithOpts(t, cache, 0, "", RequireRepoAccessOptions{AllowLegacyTokens: true})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/octocorp/widgets/1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (escape hatch on)", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Deprecation"); got != "legacy-token" {
		t.Errorf("Deprecation = %q, want %q", got, "legacy-token")
	}
	if sc.calls != 0 {
		t.Errorf("checker called %d times for legacy bypass, want 0", sc.calls)
	}
}

// TestRequireRepoAccess_UserBoundTokenAllowsOnPermission: GitHub says
// permission=read → 200, no Deprecation header.
func TestRequireRepoAccess_UserBoundTokenAllowsOnPermission(t *testing.T) {
	sc := &stubChecker{allow: true}
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 42, "alice")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Header().Get("Deprecation"); got != "" {
		t.Errorf("Deprecation = %q, want empty", got)
	}
	if sc.calls != 1 {
		t.Errorf("checker calls = %d, want 1", sc.calls)
	}
	if sc.lastUL != "alice" {
		t.Errorf("userLogin observed = %q, want alice", sc.lastUL)
	}
}

// TestRequireRepoAccess_UserBoundTokenDeniedByGitHub: 404 from the checker
// (user is not a collaborator) → 404 to the caller.
func TestRequireRepoAccess_UserBoundTokenDeniedByGitHub(t *testing.T) {
	sc := &stubChecker{allow: false} // no error: GitHub said 404
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 42, "alice")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// TestRequireRepoAccess_5xxNoCacheReturns503: cold miss + GitHub 5xx →
// 503 with Retry-After.
func TestRequireRepoAccess_5xxNoCacheReturns503(t *testing.T) {
	sc := &stubChecker{err: fmt.Errorf("%w: 503", repoaccess.ErrUnavailable)}
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 42, "alice")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want %q", got, "30")
	}
}

// TestRequireRepoAccess_5xxWithStaleAllowEmitsHeader: a prior positive entry
// + checker now returning ErrUnavailable + within grace → 200 +
// Deprecation: stale-allow.
func TestRequireRepoAccess_5xxWithStaleAllowEmitsHeader(t *testing.T) {
	sc := &stubChecker{allow: true}
	now := time.Unix(1_700_000_000, 0)
	cache := repoaccess.NewCache(sc, repoaccess.Options{NowFn: newFakeClock(&now)})
	r := rapHarness(t, cache, 42, "alice")

	// Seed positive entry.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("seed status = %d, want 200", w.Code)
	}

	// Advance past TTL but within stale-grace; flip the checker to 5xx.
	now = now.Add(10 * time.Minute)
	sc.mu.Lock()
	sc.err = fmt.Errorf("%w: 503", repoaccess.ErrUnavailable)
	sc.mu.Unlock()

	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stale-allow status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Deprecation"); got != "stale-allow" {
		t.Errorf("Deprecation = %q, want %q", got, "stale-allow")
	}
}

// TestRequireRepoAccess_403ConfigErrorReturns500: GitHub 403 means our App
// scopes are wrong — surface as 500, not 404, and DO NOT cache.
func TestRequireRepoAccess_403ConfigErrorReturns500(t *testing.T) {
	sc := &stubChecker{err: fmt.Errorf("%w: 403", repoaccess.ErrConfigError)}
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 42, "alice")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// TestRequireRepoAccess_PositiveCacheCollapsesSubsequentCalls: the second
// request for the same (user, repo) is served from cache — no checker call.
func TestRequireRepoAccess_PositiveCacheCollapsesSubsequentCalls(t *testing.T) {
	sc := &stubChecker{allow: true}
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 42, "alice")

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", i, w.Code)
		}
	}
	if sc.calls != 1 {
		t.Errorf("checker calls = %d, want 1 (cache should collapse)", sc.calls)
	}
}

// TestRequireRepoAccess_DifferentUsersDoNotCollideAtMiddleware: a positive
// entry for user A must NOT satisfy a request from user B. End-to-end check
// — the key-collision risk lives at the middleware boundary.
func TestRequireRepoAccess_DifferentUsersDoNotCollideAtMiddleware(t *testing.T) {
	sc := &stubChecker{allow: true}
	cache := repoaccess.NewCache(sc, repoaccess.Options{})

	rA := rapHarness(t, cache, 100, "alice")
	rB := rapHarness(t, cache, 200, "bob")

	// alice seeds positive entry.
	wA := httptest.NewRecorder()
	reqA, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	rA.ServeHTTP(wA, reqA)

	// bob's request to the same repo must trigger a fresh checker call —
	// alice's cached allow MUST NOT spill into bob's decision.
	wB := httptest.NewRecorder()
	reqB, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	rB.ServeHTTP(wB, reqB)

	if sc.calls != 2 {
		t.Errorf("checker calls = %d, want 2 (one per distinct user)", sc.calls)
	}
}

// TestRequireRepoAccess_NilCachePanics: the middleware refuses to construct
// itself without a cache — silently allowing every request is the worst
// possible default.
func TestRequireRepoAccess_NilCachePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil cache")
		}
	}()
	_ = RequireRepoAccess(nil, RequireRepoAccessOptions{})
}

// TestRequireRepoAccess_404BodyExposesGenericMessage: the body MUST NOT
// confirm/deny the repo's existence (info-leak).
func TestRequireRepoAccess_404BodyExposesGenericMessage(t *testing.T) {
	sc := &stubChecker{allow: false}
	cache := repoaccess.NewCache(sc, repoaccess.Options{})
	r := rapHarness(t, cache, 42, "alice")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/sse/o/r/1", nil)
	r.ServeHTTP(w, req)
	body := w.Body.String()
	if body == "" {
		t.Fatal("expected JSON error body")
	}
	// Body must not contain "permission" / "collaborator" or "octocorp"
	// (the path-param owner) — those would confirm a different access
	// decision branch to a non-member.
	lower := strings.ToLower(body)
	for _, leak := range []string{"permission", "collaborator", "octocorp"} {
		if strings.Contains(lower, leak) {
			t.Errorf("body leaks %q: %s", leak, body)
		}
	}
}
