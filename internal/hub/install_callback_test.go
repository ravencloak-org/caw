package hub

import (
	"encoding/json"
	"errors"
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

// callbackTestOpts configures what the wired InstallCallbackHandler exposes to
// each test. The fields it does not set fall back to handler defaults that mean
// "happy path."
type callbackTestOpts struct {
	// clientID/clientSecret are returned by credsFn. Both empty → ok=false (the
	// "no credentials configured" branch). credsErr overrides to err != nil.
	clientID, clientSecret string
	credsErr               error
	// mintErr forces mintFn to fail.
	mintErr error
}

// newInstallCallbackForTest wires an InstallCallbackHandler that points at a
// stub GitHub auth server. The credsFn returns opts.clientID/clientSecret/credsErr
// so tests can exercise each branch.
func newInstallCallbackForTest(t *testing.T, fake *fakeGitHubAuth, opts callbackTestOpts) (*gin.Engine, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	credsFn := func() (string, string, bool, error) {
		if opts.credsErr != nil {
			return "", "", false, opts.credsErr
		}
		if opts.clientID == "" && opts.clientSecret == "" {
			return "", "", false, nil
		}
		return opts.clientID, opts.clientSecret, true, nil
	}
	mintFn := func(installationID, org string) (string, error) {
		if opts.mintErr != nil {
			return "", opts.mintErr
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
		CredsFn:    credsFn,
		MintFn:     mintFn,
	})
	if err != nil {
		t.Fatalf("NewInstallCallbackHandler: %v", err)
	}
	r := gin.New()
	r.GET("/github/app/install/callback", h.Handle)
	return r, st
}

// happyOpts returns config that satisfies every branch up to and including
// the OAuth + ownership check, leaving the test free to override.
func happyOpts() callbackTestOpts {
	return callbackTestOpts{clientID: "Iv1.fakeclient", clientSecret: "fakesecret"}
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
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
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
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "update", "code"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestInstallCallback_MissingOAuthCode(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "OAuth") {
		t.Fatalf("body = %q, want mention of OAuth", w.Body.String())
	}
}

func TestInstallCallback_NoCreds(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, callbackTestOpts{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want 424", w.Code)
	}
}

func TestInstallCallback_PartialCreds(t *testing.T) {
	// clientID present, clientSecret empty — credsFn says ok=true, handler still rejects.
	opts := callbackTestOpts{clientID: "Iv1.fakeclient"}
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, opts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want 424", w.Code)
	}
}

func TestInstallCallback_CredsLookupError(t *testing.T) {
	opts := callbackTestOpts{credsErr: errors.New("db locked")}
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, opts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestInstallCallback_OAuthExchangeFails(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthStatus: http.StatusUnauthorized, oauthBody: `{"error":"bad_verification_code"}`}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "bad-code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInstallCallback_OAuthExchangeReturnsNoToken(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthBody: `{"access_token":"","error":"expired"}`}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInstallCallback_OAuthMalformedJSON(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthBody: `not json at all`}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInstallCallback_InstallationsAPIFails(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, installationsStatus: http.StatusServiceUnavailable}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestInstallCallback_UserNotAdminOfInstallation(t *testing.T) {
	// User is admin of installations 999, 1000 — but installation_id=123 is the request.
	fake := &fakeGitHubAuth{t: t, installationsExtraIDs: []int64{999, 1000}}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
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
	r, st := newInstallCallbackForTest(t, fake, happyOpts())
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
	opts := happyOpts()
	opts.mintErr = fmt.Errorf("disk full")
	r, _ := newInstallCallbackForTest(t, fake, opts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
