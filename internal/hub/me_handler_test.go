package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

// meHarness wires the four /me routes against a seeded store with a stub
// auth middleware that reads the user identity from query params. This
// mirrors the scope_middleware_test pattern: real auth.Required is exercised
// elsewhere (token_test.go); here we want full control over the
// (user_id, login) the handler sees so cross-user behavior is testable.
//
// Query params consumed by the stub:
//
//	?user=<int64>  → ContextGitHubUserID  (0 = legacy)
//	?login=<str>   → ContextGitHubUserLogin
type meHarness struct {
	r        *gin.Engine
	st       *store.Store
	flusher  *fakeUserFlusher
	nowValue int64
}

type fakeUserFlusher struct {
	flushed []int64
}

func (f *fakeUserFlusher) FlushUser(userID int64) {
	f.flushed = append(f.flushed, userID)
}

func newMeHarness(t *testing.T) *meHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "me.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := &meHarness{
		st:       st,
		flusher:  &fakeUserFlusher{},
		nowValue: 1_700_000_500,
	}
	mh := NewMeHandler(st, h.flusher, func() int64 { return h.nowValue })

	stubAuth := func(c *gin.Context) {
		var userID int64
		if v := c.Query("user"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				userID = n
			}
		}
		c.Set(auth.ContextGitHubUserID, userID)
		c.Set(auth.ContextGitHubUserLogin, c.Query("login"))
		c.Set(auth.ContextInstallationID, "stub")
		c.Next()
	}
	r := gin.New()
	r.GET("/me", stubAuth, mh.HandleMe)
	r.GET("/me/tokens", stubAuth, mh.HandleMeTokens)
	r.DELETE("/me/tokens/:id", stubAuth, mh.HandleMeTokenRevoke)
	r.POST("/me/recover", stubAuth, mh.HandleMeRecover)
	h.r = r
	return h
}

func doReq(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func seedToken(t *testing.T, st *store.Store, tok store.Token) {
	t.Helper()
	if err := st.InsertTokenRow(tok); err != nil {
		t.Fatalf("InsertTokenRow %s: %v", tok.Hash, err)
	}
}

// uid is a helper to build a *int64 inline.
func uid(v int64) *int64 { return &v }

// --- GET /me ---

func TestMe_LegacyTokenRejected(t *testing.T) {
	h := newMeHarness(t)
	w := doReq(h.r, http.MethodGet, "/me?user=0")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("legacy /me: status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "user-bound token required") {
		t.Errorf("body missing actionable message: %s", w.Body.String())
	}
}

func TestMe_HappyPath(t *testing.T) {
	h := newMeHarness(t)
	const userID = int64(42)
	seedToken(t, h.st, store.Token{
		ID: "T0000000000000000000000001", Hash: "h1",
		InstallationID: "inst-A", Org: "ravencloak-org",
		GitHubUserID: uid(userID), GitHubUserLogin: "octocat",
		DeviceLabel: "Claude Code @ jobin-mbp",
		CreatedAt:   1_700_000_000,
	})
	seedToken(t, h.st, store.Token{
		ID: "T0000000000000000000000002", Hash: "h2",
		InstallationID: "inst-B", Org: "other-org",
		GitHubUserID: uid(userID), GitHubUserLogin: "octocat",
		DeviceLabel: "Cursor @ laptop",
		CreatedAt:   1_700_000_001,
	})
	// Another user — must not bleed into the response.
	seedToken(t, h.st, store.Token{
		ID: "X0000000000000000000000001", Hash: "hx",
		InstallationID: "inst-A", Org: "ravencloak-org",
		GitHubUserID: uid(99), GitHubUserLogin: "stranger",
		DeviceLabel: "intruder",
		CreatedAt:   1_700_000_002,
	})

	w := doReq(h.r, http.MethodGet, "/me?user=42&login=octocat")
	if w.Code != http.StatusOK {
		t.Fatalf("/me: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got MeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	if got.GitHubUserID != userID {
		t.Errorf("user_id = %d, want %d", got.GitHubUserID, userID)
	}
	if got.GitHubUserLogin != "octocat" {
		t.Errorf("login = %q, want octocat", got.GitHubUserLogin)
	}
	if got.TokensCount != 2 {
		t.Errorf("tokens_count = %d, want 2", got.TokensCount)
	}
	if len(got.Installations) != 2 {
		t.Fatalf("installations = %d, want 2", len(got.Installations))
	}
	// Defensive: must NOT contain the other user's installation org leak.
	body := w.Body.String()
	if strings.Contains(body, "stranger") || strings.Contains(body, "intruder") {
		t.Errorf("response leaked other-user data: %s", body)
	}
	// /me MUST NOT carry token hashes, raw tokens, or token_id.
	for _, banned := range []string{"h1", "h2", "hx", "token_hash", "token\":\""} {
		if strings.Contains(body, banned) {
			t.Errorf("response leaked %q (raw=%s)", banned, body)
		}
	}
}

func TestMe_NoTokensReturnsEmpty(t *testing.T) {
	h := newMeHarness(t)
	w := doReq(h.r, http.MethodGet, "/me?user=42&login=octocat")
	if w.Code != http.StatusOK {
		t.Fatalf("/me empty: status = %d, want 200", w.Code)
	}
	var got MeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TokensCount != 0 || len(got.Installations) != 0 {
		t.Errorf("got tokens=%d installations=%d, want 0/0", got.TokensCount, len(got.Installations))
	}
}

func TestMe_RevokedTokenHiddenFromInstallationsButCounted(t *testing.T) {
	h := newMeHarness(t)
	const userID = int64(7)
	revAt := int64(1_700_000_300)
	seedToken(t, h.st, store.Token{
		ID: "R0000000000000000000000001", Hash: "hrev",
		InstallationID: "inst-revoked", Org: "ravencloak-org",
		GitHubUserID: uid(userID), GitHubUserLogin: "u7",
		DeviceLabel: "old-laptop",
		CreatedAt:   1_700_000_000,
		RevokedAt:   &revAt,
	})
	seedToken(t, h.st, store.Token{
		ID: "R0000000000000000000000002", Hash: "hactive",
		InstallationID: "inst-active", Org: "ravencloak-org",
		GitHubUserID: uid(userID), GitHubUserLogin: "u7",
		DeviceLabel: "new-laptop",
		CreatedAt:   1_700_000_010,
	})

	w := doReq(h.r, http.MethodGet, "/me?user=7&login=u7")
	var got MeResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.TokensCount != 2 {
		t.Errorf("tokens_count = %d, want 2 (revoked counted)", got.TokensCount)
	}
	if len(got.Installations) != 1 || got.Installations[0].InstallationID != "inst-active" {
		t.Errorf("installations = %+v, want only inst-active", got.Installations)
	}
}

// --- GET /me/tokens ---

func TestMeTokens_LegacyRejected(t *testing.T) {
	h := newMeHarness(t)
	w := doReq(h.r, http.MethodGet, "/me/tokens?user=0")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestMeTokens_ListHidesHashes(t *testing.T) {
	h := newMeHarness(t)
	const userID = int64(42)
	seedToken(t, h.st, store.Token{
		ID: "T0000000000000000000000001", Hash: "secret-hash-1",
		InstallationID: "inst-A", Org: "ravencloak-org",
		GitHubUserID: uid(userID), GitHubUserLogin: "octocat",
		DeviceLabel: "Claude Code @ jobin-mbp",
		CreatedAt:   1_700_000_000,
	})
	rev := int64(1_700_000_400)
	seedToken(t, h.st, store.Token{
		ID: "T0000000000000000000000002", Hash: "secret-hash-2",
		InstallationID: "inst-A", Org: "ravencloak-org",
		GitHubUserID: uid(userID), GitHubUserLogin: "octocat",
		DeviceLabel: "stale-laptop",
		CreatedAt:   1_700_000_005,
		RevokedAt:   &rev,
	})

	w := doReq(h.r, http.MethodGet, "/me/tokens?user=42&login=octocat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got MeTokensResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tokens) != 2 {
		t.Fatalf("tokens = %d, want 2", len(got.Tokens))
	}
	// Hash MUST NOT appear in the response — neither as a field nor a value.
	body := w.Body.String()
	for _, banned := range []string{"secret-hash-1", "secret-hash-2", "token_hash", `"hash"`} {
		if strings.Contains(body, banned) {
			t.Errorf("/me/tokens leaked %q in body=%s", banned, body)
		}
	}
	// Revoked row must surface its revoked_at.
	var sawRevoked bool
	for _, v := range got.Tokens {
		if v.TokenID == "T0000000000000000000000002" {
			if v.RevokedAt == nil || *v.RevokedAt != rev {
				t.Errorf("revoked row missing RevokedAt: %+v", v)
			}
			sawRevoked = true
		}
	}
	if !sawRevoked {
		t.Errorf("revoked row missing from response: %+v", got.Tokens)
	}
}

func TestMeTokens_OtherUserNotListed(t *testing.T) {
	h := newMeHarness(t)
	seedToken(t, h.st, store.Token{
		ID: "T0000000000000000000000001", Hash: "h-mine",
		InstallationID: "inst-A", GitHubUserID: uid(42),
		GitHubUserLogin: "me", DeviceLabel: "mine", CreatedAt: 1,
	})
	seedToken(t, h.st, store.Token{
		ID: "X0000000000000000000000001", Hash: "h-other",
		InstallationID: "inst-A", GitHubUserID: uid(99),
		GitHubUserLogin: "other", DeviceLabel: "other", CreatedAt: 2,
	})
	w := doReq(h.r, http.MethodGet, "/me/tokens?user=42&login=me")
	var got MeTokensResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Tokens) != 1 || got.Tokens[0].TokenID != "T0000000000000000000000001" {
		t.Errorf("isolation failure: got %+v", got.Tokens)
	}
}

// --- DELETE /me/tokens/:id ---

func TestMeTokenRevoke_LegacyRejected(t *testing.T) {
	h := newMeHarness(t)
	w := doReq(h.r, http.MethodDelete, "/me/tokens/anything?user=0")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestMeTokenRevoke_OwnTokenReturns204AndMarksRevoked(t *testing.T) {
	h := newMeHarness(t)
	const userID = int64(42)
	const id = "T0000000000000000000000001"
	seedToken(t, h.st, store.Token{
		ID: id, Hash: "h1",
		InstallationID: "inst-A", GitHubUserID: uid(userID),
		GitHubUserLogin: "octocat", DeviceLabel: "Claude Code", CreatedAt: 1,
	})

	w := doReq(h.r, http.MethodDelete, "/me/tokens/"+id+"?user=42&login=octocat")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	got, _, _ := h.st.GetTokenByID(id)
	if got.RevokedAt == nil || *got.RevokedAt != h.nowValue {
		t.Errorf("revoked_at = %v, want %d", got.RevokedAt, h.nowValue)
	}
}

func TestMeTokenRevoke_IdempotentOnAlreadyRevoked(t *testing.T) {
	h := newMeHarness(t)
	const userID = int64(42)
	const id = "T0000000000000000000000001"
	preRev := int64(1_700_000_000)
	seedToken(t, h.st, store.Token{
		ID: id, Hash: "h1",
		InstallationID: "inst-A", GitHubUserID: uid(userID),
		GitHubUserLogin: "octocat", DeviceLabel: "dev", CreatedAt: 1,
		RevokedAt: &preRev,
	})
	w := doReq(h.r, http.MethodDelete, "/me/tokens/"+id+"?user=42&login=octocat")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (idempotent); body=%s", w.Code, w.Body.String())
	}
	// Original revoked_at MUST be preserved (store.RevokeToken only fires
	// when revoked_at IS NULL); the audit timestamp is the first one.
	got, _, _ := h.st.GetTokenByID(id)
	if got.RevokedAt == nil || *got.RevokedAt != preRev {
		t.Errorf("revoked_at = %v, want preserved %d", got.RevokedAt, preRev)
	}
}

func TestMeTokenRevoke_CrossUserReturns404NotForbidden(t *testing.T) {
	h := newMeHarness(t)
	const id = "X0000000000000000000000001"
	seedToken(t, h.st, store.Token{
		ID: id, Hash: "h-other",
		InstallationID: "inst-A", GitHubUserID: uid(99),
		GitHubUserLogin: "other", DeviceLabel: "other", CreatedAt: 1,
	})

	// Caller is user 42; the row belongs to user 99 → 404, not 403.
	w := doReq(h.r, http.MethodDelete, "/me/tokens/"+id+"?user=42&login=me")
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-user revoke: status = %d, want 404 (owner identity must not leak)", w.Code)
	}
	// Defense in depth: the row MUST still be intact (not revoked).
	got, _, _ := h.st.GetTokenByID(id)
	if got.RevokedAt != nil {
		t.Errorf("other-user row was revoked: revoked_at=%v", got.RevokedAt)
	}
}

func TestMeTokenRevoke_UnknownIDReturns404(t *testing.T) {
	h := newMeHarness(t)
	w := doReq(h.r, http.MethodDelete, "/me/tokens/nope?user=42&login=me")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// --- POST /me/recover ---

func TestMeRecover_LegacyRejected(t *testing.T) {
	h := newMeHarness(t)
	w := doReq(h.r, http.MethodPost, "/me/recover?user=0")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestMeRecover_RevokesAllOwnTokensAndFlushesCache(t *testing.T) {
	h := newMeHarness(t)
	const userID = int64(42)
	for i, id := range []string{
		"A0000000000000000000000001",
		"A0000000000000000000000002",
		"A0000000000000000000000003",
	} {
		seedToken(t, h.st, store.Token{
			ID: id, Hash: "h-" + id,
			InstallationID: "inst-A", GitHubUserID: uid(userID),
			GitHubUserLogin: "octocat", DeviceLabel: "dev",
			CreatedAt: int64(1_700_000_000 + i),
		})
	}
	// Sibling user — MUST NOT be flushed or revoked.
	seedToken(t, h.st, store.Token{
		ID: "Z0000000000000000000000001", Hash: "h-other",
		InstallationID: "inst-A", GitHubUserID: uid(99),
		GitHubUserLogin: "other", DeviceLabel: "other", CreatedAt: 1,
	})

	w := doReq(h.r, http.MethodPost, "/me/recover?user=42&login=octocat")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if h.Header(w, "Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", h.Header(w, "Cache-Control"))
	}

	// Every uid=42 row is revoked.
	rows, _ := h.st.ListTokensForUser(userID)
	if len(rows) != 3 {
		t.Fatalf("rows for user = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.RevokedAt == nil || *r.RevokedAt != h.nowValue {
			t.Errorf("row %s revoked_at = %v, want %d", r.ID, r.RevokedAt, h.nowValue)
		}
	}
	// Other user untouched.
	otherRow, _, _ := h.st.GetTokenByID("Z0000000000000000000000001")
	if otherRow.RevokedAt != nil {
		t.Errorf("other-user row was revoked: %v", otherRow.RevokedAt)
	}
	// Cache flush fired for our user exactly once, with the caller's id.
	if len(h.flusher.flushed) != 1 || h.flusher.flushed[0] != userID {
		t.Errorf("flushed = %v, want [%d]", h.flusher.flushed, userID)
	}
}

func TestMeRecover_NilFlusherIsOK(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "me.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedToken(t, st, store.Token{
		ID: "T0000000000000000000000001", Hash: "h1",
		InstallationID: "inst-A", GitHubUserID: uid(42),
		GitHubUserLogin: "octocat", DeviceLabel: "dev", CreatedAt: 1,
	})

	mh := NewMeHandler(st, nil, func() int64 { return 1_700_000_500 })
	r := gin.New()
	r.POST("/me/recover", func(c *gin.Context) {
		c.Set(auth.ContextGitHubUserID, int64(42))
		c.Set(auth.ContextGitHubUserLogin, "octocat")
		mh.HandleMeRecover(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/me/recover", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

// Header is a tiny helper hung off meHarness so tests stay terse.
func (*meHarness) Header(w *httptest.ResponseRecorder, name string) string {
	return w.Result().Header.Get(name)
}
