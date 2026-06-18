package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

// fakeGitHubAuth answers the OAuth /access_token + /user/installations endpoints
// used by InstallCallbackHandler. Both are tunable per test.
type fakeGitHubAuth struct {
	t                       *testing.T
	oauthStatus             int
	oauthBody               string
	installationsStatus     int
	installationsAccountID  int64
	installationsAccountLog string
	// installationsExtraIDs lets a test inject IDs other than installationsAccountID,
	// so we can simulate "user is admin of some installations but not this one."
	installationsExtraIDs []int64
}

func (f *fakeGitHubAuth) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/oauth/access_token" && r.Method == http.MethodPost:
			status := f.oauthStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			body := f.oauthBody
			if body == "" {
				body = `{"access_token":"user-token-abc","token_type":"bearer","scope":""}`
			}
			_, _ = w.Write([]byte(body))
		case r.URL.Path == "/user/installations" && r.Method == http.MethodGet:
			status := f.installationsStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status != http.StatusOK {
				return
			}
			type acc struct {
				Login string `json:"login"`
			}
			type inst struct {
				ID      int64 `json:"id"`
				Account acc   `json:"account"`
			}
			var insts []inst
			if f.installationsAccountID != 0 {
				insts = append(insts, inst{
					ID:      f.installationsAccountID,
					Account: acc{Login: f.installationsAccountLog},
				})
			}
			for _, id := range f.installationsExtraIDs {
				insts = append(insts, inst{ID: id, Account: acc{Login: "other-org"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"installations": insts})
		default:
			f.t.Errorf("unexpected fakeGitHubAuth request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// newInstallCallbackForTest wires an InstallCallbackHandler that points at a
// stub GitHub auth server (both OAuth and REST). It also seeds App credentials
// in the store so LoadAppCredentials succeeds unless the test overrides.
func newInstallCallbackForTest(t *testing.T, fake *fakeGitHubAuth, mintErr error, seedCreds bool) (*gin.Engine, *store.Store, *httptest.Server) {
	t.Helper()
	st := newTestStore(t)
	if seedCreds {
		if err := st.SaveAppCredentials(store.AppCredentials{
			AppID:        "12345",
			ClientID:     "Iv1.fakeclient",
			ClientSecret: "fakesecret",
			PEM:          "fakepem",
		}); err != nil {
			t.Fatalf("seed credentials: %v", err)
		}
	}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	mintFn := func(installationID, org string) (string, error) {
		if mintErr != nil {
			return "", mintErr
		}
		// Mirror buildMintFn in cmd/hub/main.go: persist the hash so the
		// handler's downstream effect (token usable for /sse auth) is testable.
		const raw = "raw-watcher-token-XYZ"
		if err := st.InsertToken(auth.HashToken(raw), installationID, org); err != nil {
			return "", err
		}
		return raw, nil
	}
	h, err := NewInstallCallbackHandler(InstallCallbackConfig{
		BaseURL:    "http://hub.example.com",
		GithubBase: srv.URL, // both endpoints multiplexed by path on the stub
		APIBase:    srv.URL,
		Store:      st,
		MintFn:     mintFn,
	})
	if err != nil {
		t.Fatalf("NewInstallCallbackHandler: %v", err)
	}
	r := gin.New()
	r.GET("/github/app/install/callback", h.Handle)
	return r, st, srv
}

func installReq(installID, setupAction, code string) *http.Request {
	parts := make([]string, 0, 3)
	if installID != "" {
		parts = append(parts, "installation_id="+installID)
	}
	if setupAction != "" {
		parts = append(parts, "setup_action="+setupAction)
	}
	if code != "" {
		parts = append(parts, "code="+code)
	}
	target := "/github/app/install/callback"
	if len(parts) > 0 {
		target += "?" + strings.Join(parts, "&")
	}
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func TestInstallCallback_MissingInstallationID(t *testing.T) {
	r, _, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("", "install", "code123"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "installation_id") {
		t.Fatalf("body = %q, want mention of installation_id", w.Body.String())
	}
}

func TestInstallCallback_WrongSetupAction(t *testing.T) {
	r, _, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "update", "code"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestInstallCallback_MissingOAuthCode(t *testing.T) {
	r, _, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OAuth") {
		t.Fatalf("body = %q, want mention of OAuth", w.Body.String())
	}
}

func TestInstallCallback_NoAppCredentials(t *testing.T) {
	r, _, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, nil, false /*seedCreds*/)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want 424", w.Code)
	}
}

func TestInstallCallback_OAuthExchangeFails(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthStatus: http.StatusUnauthorized, oauthBody: `{"error":"bad_verification_code"}`}
	r, _, _ := newInstallCallbackForTest(t, fake, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "bad-code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInstallCallback_OAuthExchangeReturnsNoToken(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthBody: `{"access_token":"","error":"expired"}`}
	r, _, _ := newInstallCallbackForTest(t, fake, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInstallCallback_UserNotAdminOfInstallation(t *testing.T) {
	// User is admin of installations 999, 1000 — but installation_id=123 is the request.
	fake := &fakeGitHubAuth{t: t, installationsExtraIDs: []int64{999, 1000}}
	r, _, _ := newInstallCallbackForTest(t, fake, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestInstallCallback_HappyPath(t *testing.T) {
	fake := &fakeGitHubAuth{
		t:                       t,
		installationsAccountID:  123,
		installationsAccountLog: "ravencloak-org",
	}
	r, st, _ := newInstallCallbackForTest(t, fake, nil, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "good-code"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "raw-watcher-token-XYZ") {
		t.Errorf("body missing minted token; body=%q", body)
	}
	if !strings.Contains(body, "ravencloak-org") {
		t.Errorf("body missing account login; body=%q", body)
	}
	if !strings.Contains(body, "CAW_WATCHER_TOKEN") {
		t.Errorf("body missing harness config snippet; body=%q", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rp := w.Header().Get("Referrer-Policy"); rp != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", rp)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("Content-Security-Policy missing default-src 'self'; got %q", csp)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	// Token was persisted (by hash) — VerifyToken finds it.
	installID, ok, err := st.VerifyToken(auth.HashToken("raw-watcher-token-XYZ"))
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if !ok {
		t.Fatalf("minted token not present in store")
	}
	if installID != "123" {
		t.Errorf("token installation = %q, want 123", installID)
	}
}

func TestInstallCallback_MintFails(t *testing.T) {
	fake := &fakeGitHubAuth{
		t:                       t,
		installationsAccountID:  123,
		installationsAccountLog: "ravencloak-org",
	}
	r, _, _ := newInstallCallbackForTest(t, fake, fmt.Errorf("disk full"), true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
