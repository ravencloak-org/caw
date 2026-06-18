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
	// captured records the args mintFn was invoked with so tests can assert
	// the widened Phase 1 signature.
	captured *mintCall
}

// mintCall is the recorded mintFn invocation: install_callback Phase 1
// passes deviceLabel="legacy", userID=0, userLogin="" (legacy semantics).
type mintCall struct {
	installationID string
	org            string
	deviceLabel    string
	userID         int64
	userLogin      string
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
	mintFn := func(installationID, org, deviceLabel string, userID int64, userLogin string) (string, string, error) {
		if opts.captured != nil {
			*opts.captured = mintCall{installationID, org, deviceLabel, userID, userLogin}
		}
		if opts.mintErr != nil {
			return "", "", opts.mintErr
		}
		// Mirror buildMintFn in cmd/hub/main.go: persist the hash so the
		// handler's downstream effect (token usable for /sse auth) is testable.
		const raw = "raw-watcher-token-XYZ"
		if err := st.InsertTokenRow(store.Token{
			ID:             "01HXFAKETOKENIDABCDEFGHJKM",
			Hash:           auth.HashToken(raw),
			InstallationID: installationID,
			Org:            org,
			DeviceLabel:    deviceLabel,
		}); err != nil {
			return "", "", err
		}
		return raw, "01HXFAKETOKENIDABCDEFGHJKM", nil
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

// assertErrorPage verifies w carries an install_error.html render at the given
// status with the expected error-code badge + each wantSubstrings literal. Used
// by every failure-path test so the bare-text-body assertions stay gone.
func assertErrorPage(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string, wantSubstrings ...string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%q", w.Code, wantStatus, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "<!DOCTYPE html>") {
		t.Errorf("body does not start with <!DOCTYPE html>; first 80 bytes=%q", firstN(body, 80))
	}
	// Error code is rendered in a <code> badge in the sub-line.
	if wantCode != "" && !strings.Contains(body, ">"+wantCode+"<") {
		t.Errorf("body missing rendered error code %q; body=%q", wantCode, body)
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(body, s) {
			t.Errorf("body missing substring %q; body=%q", s, body)
		}
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestInstallCallback_MissingInstallationID(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("", "install", "code123"))
	assertErrorPage(t, w, http.StatusBadRequest, "missing_installation_id", "installation_id", "Restart login")
}

func TestInstallCallback_WrongSetupAction(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	// "update" is now a valid soft-redirect (see TestInstallCallback_SetupActionUpdate);
	// only genuinely unknown values trigger this branch.
	r.ServeHTTP(w, installReq("123", "garbage", "code"))
	assertErrorPage(t, w, http.StatusBadRequest, "unsupported_setup_action", "setup_action=garbage")
}

func TestInstallCallback_MissingOAuthCode(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", ""))
	assertErrorPage(t, w, http.StatusBadRequest, "missing_oauth_code",
		"OAuth", `Request user authorization (OAuth)`)
}

func TestInstallCallback_NoCreds(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, callbackTestOpts{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusFailedDependency, "no_credentials",
		"CAW_APP_CLIENT_ID", "manifest")
}

func TestInstallCallback_PartialCreds(t *testing.T) {
	// clientID present, clientSecret empty — credsFn says ok=true, handler still rejects.
	opts := callbackTestOpts{clientID: "Iv1.fakeclient"}
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, opts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusFailedDependency, "no_credentials")
}

func TestInstallCallback_CredsLookupError(t *testing.T) {
	opts := callbackTestOpts{credsErr: errors.New("db locked")}
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, opts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusInternalServerError, "creds_lookup_failed")
}

func TestInstallCallback_OAuthExchangeFails(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthStatus: http.StatusUnauthorized, oauthBody: `{"error":"bad_verification_code"}`}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "bad-code"))
	assertErrorPage(t, w, http.StatusBadGateway, "oauth_exchange_failed", "OAuth")
}

func TestInstallCallback_OAuthExchangeReturnsNoToken(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthBody: `{"access_token":"","error":"expired"}`}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusBadGateway, "oauth_exchange_failed")
}

func TestInstallCallback_OAuthMalformedJSON(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, oauthBody: `not json at all`}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusBadGateway, "oauth_exchange_failed")
}

func TestInstallCallback_InstallationsAPIFails(t *testing.T) {
	fake := &fakeGitHubAuth{t: t, installationsStatus: http.StatusServiceUnavailable}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusBadGateway, "installations_lookup_failed")
}

func TestInstallCallback_UserNotAdminOfInstallation(t *testing.T) {
	// User is admin of installations 999, 1000 — but installation_id=123 is the request.
	fake := &fakeGitHubAuth{t: t, installationsExtraIDs: []int64{999, 1000}}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusForbidden, "not_an_admin", "admin")
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
	// Token was persisted (by hash) — VerifyToken finds it; Phase 1 keeps
	// install-callback mint calls on legacy semantics (GitHubUserID == nil,
	// DeviceLabel == "legacy") via the widened MintFunc.
	tok, ok, err := st.VerifyToken(auth.HashToken("raw-watcher-token-XYZ"))
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if !ok {
		t.Fatalf("minted token not present in store")
	}
	if tok.InstallationID != "123" {
		t.Errorf("token installation = %q, want 123", tok.InstallationID)
	}
	if tok.GitHubUserID != nil {
		t.Errorf("install-callback Phase 1 GitHubUserID = %v, want nil (legacy)", *tok.GitHubUserID)
	}
	if tok.DeviceLabel != "legacy" {
		t.Errorf("install-callback Phase 1 DeviceLabel = %q, want \"legacy\"", tok.DeviceLabel)
	}
}

// TestInstallCallback_HappyPath_MintFnReceivesWidenedArgs verifies the
// install-callback widens its MintFunc call per Phase 1: deviceLabel="legacy",
// userID=0, userLogin="". Phase 3's /auth/picker handler is the path that
// will start passing real user values; the install callback intentionally
// stays on legacy semantics until Phase 5 sunsets it.
func TestInstallCallback_HappyPath_MintFnReceivesWidenedArgs(t *testing.T) {
	fake := &fakeGitHubAuth{
		t:                       t,
		installationsAccountID:  321,
		installationsAccountLog: "ravencloak-org",
	}
	var captured mintCall
	opts := happyOpts()
	opts.captured = &captured
	r, _ := newInstallCallbackForTest(t, fake, opts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("321", "install", "good-code"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if captured.installationID != "321" {
		t.Errorf("mintFn installationID = %q, want 321", captured.installationID)
	}
	if captured.org != "ravencloak-org" {
		t.Errorf("mintFn org = %q, want ravencloak-org", captured.org)
	}
	if captured.deviceLabel != "legacy" {
		t.Errorf("mintFn deviceLabel = %q, want \"legacy\"", captured.deviceLabel)
	}
	if captured.userID != 0 {
		t.Errorf("mintFn userID = %d, want 0 (legacy)", captured.userID)
	}
	if captured.userLogin != "" {
		t.Errorf("mintFn userLogin = %q, want \"\"", captured.userLogin)
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
	assertErrorPage(t, w, http.StatusInternalServerError, "mint_failed")
}

// TestInstallCallback_SetupActionUpdate covers GitHub's setup_action=update
// redirect (existing installation reconfigured — e.g. repo added/removed).
// Behavior changed in Phase 0 of Auth v2: was 400 bare text, now 200 HTML
// soft-redirect that points the user at /me/tokens (Phase 4 route).
func TestInstallCallback_SetupActionUpdate(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("139674548", "update", "ignored"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/me/tokens") {
		t.Errorf("body missing /me/tokens link; body=%q", body)
	}
	if !strings.Contains(body, "hub mint-token") {
		t.Errorf("body missing self-host rotate hint; body=%q", body)
	}
	// The button URL is rooted at the hub baseURL (http://hub.example.com in tests).
	if !strings.Contains(body, `href="http://hub.example.com/me/tokens"`) {
		t.Errorf("body missing absolute /me/tokens href; body=%q", body)
	}
}

// TestInstallCallback_ErrorPageRendersForMissingInstallationID asserts the
// install_error.html template renders with all the actionable affordances:
// title, error-code badge, restart-login button, self-host docs reference.
// Complements TestInstallCallback_MissingInstallationID, which is a thinner
// status+substring check.
func TestInstallCallback_ErrorPageRendersForMissingInstallationID(t *testing.T) {
	r, _ := newInstallCallbackForTest(t, &fakeGitHubAuth{t: t}, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("", "install", "code"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Template skeleton.
	for _, must := range []string{
		"<!DOCTYPE html>",
		`<title>caw — `,
		`<h2>What happened</h2>`,
		`<h2>What to do</h2>`,
		// Restart-login button is the prominent CTA.
		`<a class="btn" href="http://hub.example.com/auth/start-help">Restart login</a>`,
		// Error code badge.
		`<code>missing_installation_id</code>`,
		// Self-host docs anchor lives in the footer of every rendered error.
		`docs/install/SELF-HOST.md`,
	} {
		if !strings.Contains(body, must) {
			t.Errorf("body missing %q; body=%q", must, body)
		}
	}
	// No-store CSP without script-src — error template has no inline JS.
	if csp := w.Header().Get("Content-Security-Policy"); strings.Contains(csp, "script-src") {
		t.Errorf("error page CSP allows script-src; should not (got %q)", csp)
	}
}

// TestInstallCallback_ErrorPageRendersForFailedOAuth asserts the OAuth-failure
// path renders the actionable HTML page rather than the old "OAuth exchange
// failed" bare-text body. Covers both the 502 status and the restart-login CTA.
func TestInstallCallback_ErrorPageRendersForFailedOAuth(t *testing.T) {
	fake := &fakeGitHubAuth{
		t:           t,
		oauthStatus: http.StatusUnauthorized,
		oauthBody:   `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`,
	}
	r, _ := newInstallCallbackForTest(t, fake, happyOpts())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "stale-code"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := w.Body.String()
	for _, must := range []string{
		"<!DOCTYPE html>",
		`<code>oauth_exchange_failed</code>`,
		"OAuth",
		// Actionable copy explicitly mentions the most-likely cause.
		"expired",
		// Restart-login button.
		`<a class="btn" href="http://hub.example.com/auth/start-help">Restart login</a>`,
	} {
		if !strings.Contains(body, must) {
			t.Errorf("body missing %q; body=%q", must, body)
		}
	}
}

func TestNewInstallCallbackHandler_EmptyBaseURL(t *testing.T) {
	_, err := NewInstallCallbackHandler(InstallCallbackConfig{
		CredsFn: func() (string, string, bool, error) { return "id", "sec", true, nil },
		MintFn:  MintFunc(func(string, string, string, int64, string) (string, string, error) { return "tok", "id", nil }),
	})
	if err == nil {
		t.Fatal("want error for empty BaseURL")
	}
	if !strings.Contains(err.Error(), "BaseURL") {
		t.Errorf("error = %v, want mention of BaseURL", err)
	}
}

func TestNewInstallCallbackHandler_NilCredsFn(t *testing.T) {
	_, err := NewInstallCallbackHandler(InstallCallbackConfig{
		BaseURL: "http://h.example",
		MintFn:  MintFunc(func(string, string, string, int64, string) (string, string, error) { return "tok", "id", nil }),
	})
	if err == nil {
		t.Fatal("want error for nil CredsFn")
	}
	if !strings.Contains(err.Error(), "CredsFn") {
		t.Errorf("error = %v, want mention of CredsFn", err)
	}
}

func TestNewInstallCallbackHandler_NilMintFn(t *testing.T) {
	_, err := NewInstallCallbackHandler(InstallCallbackConfig{
		BaseURL: "http://h.example",
		CredsFn: func() (string, string, bool, error) { return "id", "sec", true, nil },
	})
	if err == nil {
		t.Fatal("want error for nil MintFn")
	}
	if !strings.Contains(err.Error(), "MintFn") {
		t.Errorf("error = %v, want mention of MintFn", err)
	}
}

// fakeGitHubAuthMalformed is a separate stub because fakeGitHubAuth
// always JSON-encodes its installations body; we need to inject raw
// garbage for the installations decode-error branch.
func TestInstallCallback_InstallationsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"t"}`))
		case "/user/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json at all`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	st := newTestStore(t)
	credsFn := func() (string, string, bool, error) { return "id", "sec", true, nil }
	mintFn := func(installationID, org, deviceLabel string, _ int64, _ string) (string, string, error) {
		const raw = "raw-watcher-token-XYZ"
		return raw, "01HXFAKETOKENIDABCDEFGHJKM", st.InsertTokenRow(store.Token{
			ID:             "01HXFAKETOKENIDABCDEFGHJKM",
			Hash:           auth.HashToken(raw),
			InstallationID: installationID,
			Org:            org,
			DeviceLabel:    deviceLabel,
		})
	}
	h, err := NewInstallCallbackHandler(InstallCallbackConfig{
		BaseURL:    "http://hub.example.com",
		GithubBase: srv.URL,
		APIBase:    srv.URL,
		CredsFn:    credsFn,
		MintFn:     mintFn,
	})
	if err != nil {
		t.Fatalf("NewInstallCallbackHandler: %v", err)
	}
	r := gin.New()
	r.GET("/github/app/install/callback", h.Handle)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, installReq("123", "install", "code"))
	assertErrorPage(t, w, http.StatusBadGateway, "installations_lookup_failed")
}
