package server_test

// End-to-end Auth v2 Phase 2 acceptance gates. These exercise the full Hub
// HTTP surface (server.New) with the new RequireRepoAccess middleware wired
// after auth.Required + RequireRepoScope on /sse/owner/repo/:n + /leases/...:
//
//   - Legacy (NULL github_user_id) tokens BYPASS with `Deprecation: legacy-token`.
//   - User-bound tokens HIT the cache → on miss, ask the stub Checker → cache
//     positive 5 min / negative 60 s, stale-allow up to 30 min on 5xx.
//   - The webhook `installation_repositories.removed` event MUST flush any
//     positive cache entries for that (installation, repo) so a token whose
//     installation just lost access cannot keep tailing the repo.
//
// We isolate the test harness from server_test.go's newTestServer so we can
// (a) inject a user-bound token row, (b) hand the server a Cache backed by
// a deterministic stub Checker, and (c) use numeric installation IDs that
// align between the token row and webhook envelopes (webhook ingest writes
// installation.id as its decimal string into the store).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/repoaccess"
	"github.com/ravencloak-org/caw/internal/server"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

// stubChecker is a controllable repoaccess.Checker for the e2e tests.
type stubChecker struct {
	mu    sync.Mutex
	allow bool
	err   error
	calls int
}

func (s *stubChecker) HasReadAccess(_ context.Context, _, _, _, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.allow, s.err
}

func (s *stubChecker) setResult(allow bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allow = allow
	s.err = err
}

func (s *stubChecker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// authHarness builds a Hub server with one legacy and one user-bound token
// scoped to installation "1" / repo o/r. Installation id is the decimal
// string of an int64 (matches the webhook ingest path, so flush-triggering
// webhook payloads operate on the SAME installation row the tokens use).
type authHarness struct {
	ts          *httptest.Server
	st          *store.Store
	cache       *repoaccess.Cache
	checker     *stubChecker
	legacyToken string
	userToken   string
}

// installID is the numeric installation id used throughout the auth harness.
// Stored as its decimal string everywhere a string is required.
const harnessInstallID = int64(101)

func harnessInstallIDStr() string { return fmt.Sprintf("%d", harnessInstallID) }

func newAuthHarness(t *testing.T, nowFn func() time.Time) *authHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertInstallation(harnessInstallIDStr(), "org1"); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	if err := st.AddInstallationRepo(harnessInstallIDStr(), "o/r"); err != nil {
		t.Fatalf("add installation repo: %v", err)
	}

	// Legacy token: GitHubUserID nil → Phase 2 bypass with Deprecation hdr.
	legacyRaw, legacyHash, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("legacy token: %v", err)
	}
	if err := st.InsertTokenRow(store.Token{
		Hash:           legacyHash,
		InstallationID: harnessInstallIDStr(),
		Org:            "org1",
		DeviceLabel:    "legacy",
	}); err != nil {
		t.Fatalf("insert legacy token: %v", err)
	}

	// User-bound token: GitHubUserID = 42 → hits RequireRepoAccess for real.
	userID := int64(42)
	userRaw, userHash, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("user token: %v", err)
	}
	if err := st.InsertTokenRow(store.Token{
		ID:              "01HKE2EUSERTOKENID00000000",
		Hash:            userHash,
		InstallationID:  harnessInstallIDStr(),
		Org:             "org1",
		GitHubUserID:    &userID,
		GitHubUserLogin: "alice",
		DeviceLabel:     "test-device",
	}); err != nil {
		t.Fatalf("insert user-bound token: %v", err)
	}

	sc := &stubChecker{allow: true}
	cache := repoaccess.NewCache(sc, repoaccess.Options{NowFn: nowFn})

	sseHub := sse.New()
	engine := settle.New(st, sseHub, 30*time.Millisecond)
	ts := httptest.NewServer(server.New(st, sseHub, engine, []byte(secret), nil, nil, nil, cache))
	t.Cleanup(ts.Close)

	return &authHarness{
		ts:          ts,
		st:          st,
		cache:       cache,
		checker:     sc,
		legacyToken: legacyRaw,
		userToken:   userRaw,
	}
}

// openSSE issues an authenticated GET on /sse/o/r/1 and returns the response
// without consuming the body — caller is responsible for closing.
func openSSE(t *testing.T, base, token string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sse/o/r/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	return resp
}

// drainAndClose reads a small chunk and closes — lets us verify status +
// headers on an SSE stream without keeping it pinned past test teardown
// (which races deferred httptest.Close under -race).
func drainAndClose(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 256)
	_ = resp.Body.Close()
}

// TestE2E_LegacyTokenBypassesAuthV2_DeprecationHeader: legacy tokens (rows
// with NULL github_user_id, every v0.1.x prod token) continue to receive 200
// on /sse/owner/repo/n with `Deprecation: legacy-token`. This is the headline
// backward-compat guarantee of Phase 2.
func TestE2E_LegacyTokenBypassesAuthV2_DeprecationHeader(t *testing.T) {
	h := newAuthHarness(t, nil)

	resp := openSSE(t, h.ts.URL, h.legacyToken)
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy SSE status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Deprecation"); got != "legacy-token" {
		t.Errorf("Deprecation header = %q, want %q", got, "legacy-token")
	}
	// The legacy bypass MUST NOT consult the Checker — Phase 2's whole
	// point is that legacy tokens cost zero GitHub calls.
	if got := h.checker.callCount(); got != 0 {
		t.Errorf("checker calls for legacy token = %d, want 0", got)
	}
}

// TestE2E_UserBoundTokenAllowsOnReadPermission: a user-bound token + stub
// Checker returning allow=true must connect (200, no Deprecation hdr) and
// trigger exactly one Checker call.
func TestE2E_UserBoundTokenAllowsOnReadPermission(t *testing.T) {
	h := newAuthHarness(t, nil)

	resp := openSSE(t, h.ts.URL, h.userToken)
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user SSE status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Deprecation"); got != "" {
		t.Errorf("Deprecation header = %q, want empty (fresh allow)", got)
	}
	if got := h.checker.callCount(); got != 1 {
		t.Errorf("checker calls = %d, want 1", got)
	}
}

// TestE2E_UserBoundTokenDeniedReturns404: stub Checker says "no access"
// (mirrors GitHub 404) → /sse/o/r/1 returns 404.
func TestE2E_UserBoundTokenDeniedReturns404(t *testing.T) {
	h := newAuthHarness(t, nil)
	h.checker.setResult(false, nil)

	resp := openSSE(t, h.ts.URL, h.userToken)
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("denied SSE status = %d, want 404", resp.StatusCode)
	}
}

// TestE2E_UserBoundTokenUnavailableReturns503: cold cache + Checker returns
// ErrUnavailable (mirrors GitHub 5xx) → 503 with Retry-After.
func TestE2E_UserBoundTokenUnavailableReturns503(t *testing.T) {
	h := newAuthHarness(t, nil)
	h.checker.setResult(false, fmt.Errorf("%w: 503", repoaccess.ErrUnavailable))

	resp := openSSE(t, h.ts.URL, h.userToken)
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("5xx SSE status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
}

// TestE2E_UserBoundTokenStaleAllowReturns200WithDeprecation: pre-warm the
// cache with a positive entry, advance the fake clock past the 5-min TTL but
// within the 30-min stale-grace, flip the Checker to ErrUnavailable, and
// verify the second request still 200s with `Deprecation: stale-allow`.
func TestE2E_UserBoundTokenStaleAllowReturns200WithDeprecation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return now }
	h := newAuthHarness(t, nowFn)

	resp := openSSE(t, h.ts.URL, h.userToken)
	drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed status = %d, want 200", resp.StatusCode)
	}

	// Advance past 5-min TTL but well inside 30-min grace; flip to 5xx.
	now = now.Add(10 * time.Minute)
	h.checker.setResult(true, fmt.Errorf("%w: 503", repoaccess.ErrUnavailable))

	resp = openSSE(t, h.ts.URL, h.userToken)
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale-allow status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Deprecation"); got != "stale-allow" {
		t.Errorf("Deprecation = %q, want stale-allow", got)
	}
}

// TestE2E_WebhookInstallationRepositoriesRemovedFlushesCache: drive
// `installation_repositories.removed` and verify the pre-cached positive
// entry is evicted at the cache layer. We assert on cache.Len() (and the
// subsequent SSE response code) because RequireRepoScope ALSO drops the
// repo from this installation's scope and would 403 the next request
// regardless of whether the cache was flushed; the direct cache assertion
// is the one that pins down the flush hook actually fired.
func TestE2E_WebhookInstallationRepositoriesRemovedFlushesCache(t *testing.T) {
	h := newAuthHarness(t, nil)

	// Seed: first request is a miss-allow, caches positive.
	resp := openSSE(t, h.ts.URL, h.userToken)
	drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed status = %d, want 200", resp.StatusCode)
	}
	if h.checker.callCount() != 1 {
		t.Fatalf("seed checker calls = %d, want 1", h.checker.callCount())
	}
	if h.cache.Len() != 1 {
		t.Fatalf("seed cache.Len() = %d, want 1", h.cache.Len())
	}

	// Sanity: second request is a cache hit (no new Checker call).
	resp = openSSE(t, h.ts.URL, h.userToken)
	drainAndClose(resp)
	if h.checker.callCount() != 1 {
		t.Fatalf("hit checker calls = %d, want 1 (cached)", h.checker.callCount())
	}

	// Fire the webhook that evicts (harnessInstallID, "o/r") from the cache
	// and removes the repo from the installation's scope.
	postWebhook(t, h.ts.URL, "installation_repositories", "drm-1", installReposRemovedPayload(harnessInstallID, "o/r"))

	// The cache flush is the contract Phase 2 owns — assert it directly.
	if h.cache.Len() != 0 {
		t.Fatalf("post-webhook cache.Len() = %d, want 0 (cache must be flushed by webhook)", h.cache.Len())
	}

	// As a defense-in-depth sanity check, the next SSE request must NOT
	// silently 200 from a stale cached allow. RequireRepoScope now also
	// rejects (repo no longer in installation), so the user-visible code
	// is 403 — either way, a stale 200 would mean the flush silently
	// failed AND scope removal also failed, which would be a doubly bad
	// regression.
	resp = openSSE(t, h.ts.URL, h.userToken)
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("post-flush status = 200; expected non-2xx (cache + scope both blew)")
	}
}

// TestE2E_WebhookInstallationDeletedFlushesEverything: `installation.deleted`
// must drop every cache entry tied to that installation.
func TestE2E_WebhookInstallationDeletedFlushesEverything(t *testing.T) {
	h := newAuthHarness(t, nil)

	resp := openSSE(t, h.ts.URL, h.userToken)
	drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed status = %d, want 200", resp.StatusCode)
	}
	if h.cache.Len() == 0 {
		t.Fatal("cache empty after seed — expected positive entry")
	}

	postWebhook(t, h.ts.URL, "installation", "del-1", installationDeletedPayload(harnessInstallID))

	if h.cache.Len() != 0 {
		t.Errorf("cache.Len() after installation.deleted = %d, want 0", h.cache.Len())
	}
}

// installReposRemovedPayload builds the JSON envelope for an
// `installation_repositories` event with `repositories_removed`.
func installReposRemovedPayload(installID int64, fullName string) []byte {
	type repo struct {
		FullName string `json:"full_name"`
	}
	type inst struct {
		ID int64 `json:"id"`
	}
	body := map[string]any{
		"action":               "removed",
		"installation":         inst{ID: installID},
		"repositories_removed": []repo{{FullName: fullName}},
	}
	b, _ := json.Marshal(body)
	return b
}

// installationDeletedPayload builds the JSON envelope for an
// `installation.deleted` event.
func installationDeletedPayload(installID int64) []byte {
	type inst struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	i := inst{ID: installID}
	i.Account.Login = "org1"
	body := map[string]any{
		"action":       "deleted",
		"installation": i,
	}
	b, _ := json.Marshal(body)
	return b
}
