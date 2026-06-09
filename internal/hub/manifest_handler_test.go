package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/store"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// testBootstrapToken is the operator secret the test handlers are gated behind.
const testBootstrapToken = "boot-secret-123"

// newTestStore opens an in-memory store for use in manifest tests.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	_ = f.Close()
	st, err := store.Open(f.Name())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// manifestTestOpts configures the handler built by newManifestHandlerForTest.
type manifestTestOpts struct {
	githubBase       string
	mintFn           func(string, string) (string, error)
	allowRebootstrap bool
}

// newManifestHandlerForTest constructs a ManifestHandler (gated by
// testBootstrapToken) and wires it into a Gin engine for HTTP testing.
func newManifestHandlerForTest(t *testing.T, opts manifestTestOpts) (*gin.Engine, *ManifestHandler, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	mh, err := NewManifestHandler(ManifestConfig{
		BaseURL:          "http://hub.example.com",
		GithubBase:       opts.githubBase,
		Store:            st,
		MintFn:           opts.mintFn,
		BootstrapToken:   testBootstrapToken,
		AllowRebootstrap: opts.allowRebootstrap,
	})
	if err != nil {
		t.Fatalf("NewManifestHandler: %v", err)
	}
	r := gin.New()
	r.GET("/github/app/manifest", mh.HandleManifest)
	r.GET("/github/app/callback", mh.HandleCallback)
	return r, mh, st
}

// manifestReq builds a GET /github/app/manifest request. When token is
// non-empty it is carried as the operator bootstrap bearer token.
func manifestReq(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// callbackReq builds a GET /github/app/callback request with the given code and
// state query params and, when cookieState is non-empty, the state cookie that
// HandleManifest would have set.
func callbackReq(code, queryState, cookieState string) *http.Request {
	parts := make([]string, 0, 2)
	if code != "" {
		parts = append(parts, "code="+code)
	}
	if queryState != "" {
		parts = append(parts, "state="+queryState)
	}
	target := "/github/app/callback"
	if len(parts) > 0 {
		target += "?" + strings.Join(parts, "&")
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if cookieState != "" {
		req.AddCookie(&http.Cookie{Name: stateCookieName, Value: cookieState})
	}
	return req
}

// fakeGitHubSuccess returns an httptest server that answers the manifest
// conversion with valid App credentials.
func fakeGitHubSuccess(t *testing.T, appID int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             appID,
			"client_id":      "Iv1.abc",
			"client_secret":  "secret-cs",
			"webhook_secret": "secret-ws",
			"pem":            "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedCredentials persists a placeholder App credential so overwrite-protection
// paths can be exercised.
func seedCredentials(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.SaveAppCredentials(store.AppCredentials{
		AppID:         "999",
		ClientID:      "Iv1.existing",
		ClientSecret:  "cs",
		WebhookSecret: "ws",
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nexisting\n-----END RSA PRIVATE KEY-----\n",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
}

func TestNewManifestHandler_RequiresBaseURL(t *testing.T) {
	st := newTestStore(t)
	_, err := NewManifestHandler(ManifestConfig{Store: st, BootstrapToken: testBootstrapToken})
	if err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

func TestNewManifestHandler_RequiresStore(t *testing.T) {
	_, err := NewManifestHandler(ManifestConfig{BaseURL: "http://hub.example.com", BootstrapToken: testBootstrapToken})
	if err == nil {
		t.Fatal("expected error for nil Store")
	}
}

// TestNewManifestHandler_RequiresBootstrapToken verifies the credential-minting
// routes cannot be constructed without an operator bootstrap secret.
func TestNewManifestHandler_RequiresBootstrapToken(t *testing.T) {
	st := newTestStore(t)
	_, err := NewManifestHandler(ManifestConfig{BaseURL: "http://hub.example.com", Store: st})
	if err == nil {
		t.Fatal("expected error for missing BootstrapToken")
	}
}

func TestNewManifestHandler_DefaultGithubBase(t *testing.T) {
	st := newTestStore(t)
	mh, err := NewManifestHandler(ManifestConfig{
		BaseURL:        "http://hub.example.com",
		Store:          st,
		BootstrapToken: testBootstrapToken,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mh.githubBase != "https://github.com" {
		t.Errorf("githubBase = %q, want %q", mh.githubBase, "https://github.com")
	}
}

// TestHandleManifest_RequiresBootstrapAuth verifies the manifest route is gated:
// an unauthenticated request is rejected with 401.
func TestHandleManifest_RequiresBootstrapAuth(t *testing.T) {
	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, manifestReq("")) // no token

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestHandleManifest_WrongBootstrapToken verifies a bad token is rejected.
func TestHandleManifest_WrongBootstrapToken(t *testing.T) {
	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, manifestReq("wrong-token"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestHandleManifest_HTML verifies the authorized manifest endpoint returns the
// self-submitting form and sets a CSRF state cookie.
func TestHandleManifest_HTML(t *testing.T) {
	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, manifestReq(testBootstrapToken))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "action=") {
		t.Error("response should contain a form action attribute")
	}
	if !strings.Contains(body, `name="manifest"`) {
		t.Error("response should contain a manifest hidden input")
	}
	if !strings.Contains(body, "installation") {
		t.Error("manifest JSON should include 'installation' event")
	}

	var stateCookie *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == stateCookieName {
			stateCookie = ck
		}
	}
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatal("manifest response should set a non-empty CSRF state cookie")
	}
	if !stateCookie.HttpOnly {
		t.Error("state cookie should be HttpOnly")
	}
}

// TestHandleManifest_RefusesOverwrite verifies that, once credentials exist, the
// authorized manifest route refuses to overwrite them unless re-bootstrap is on.
func TestHandleManifest_RefusesOverwrite(t *testing.T) {
	r, _, st := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})
	seedCredentials(t, st)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, manifestReq(testBootstrapToken))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// TestHandleManifest_AllowsRebootstrap verifies that AllowRebootstrap permits
// overwriting existing credentials.
func TestHandleManifest_AllowsRebootstrap(t *testing.T) {
	r, _, st := newManifestHandlerForTest(t, manifestTestOpts{
		githubBase:       "https://github.test",
		allowRebootstrap: true,
	})
	seedCredentials(t, st)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, manifestReq(testBootstrapToken))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleManifest_ManifestJSON verifies the embedded manifest contains the
// expected hook URL, redirect URL, and default_events.
func TestHandleManifest_ManifestJSON(t *testing.T) {
	st := newTestStore(t)
	mh, err := NewManifestHandler(ManifestConfig{
		BaseURL:        "http://hub.example.com",
		Store:          st,
		BootstrapToken: testBootstrapToken,
	})
	if err != nil {
		t.Fatalf("NewManifestHandler: %v", err)
	}

	var manifest map[string]any
	if err := json.Unmarshal(mh.manifestJSON, &manifest); err != nil {
		t.Fatalf("manifest JSON is invalid: %v", err)
	}
	hook, ok := manifest["hook_attributes"].(map[string]any)
	if !ok {
		t.Fatal("manifest missing hook_attributes")
	}
	if hook["url"] != "http://hub.example.com/webhooks/github" {
		t.Errorf("hook url = %v", hook["url"])
	}
	if manifest["redirect_url"] != "http://hub.example.com/github/app/callback" {
		t.Errorf("redirect_url = %v", manifest["redirect_url"])
	}
	events, _ := manifest["default_events"].([]any)
	var found bool
	for _, e := range events {
		if e == "installation" {
			found = true
		}
	}
	if !found {
		t.Error("manifest default_events should include 'installation'")
	}
}

// TestHandleCallback_MissingStateCookie verifies the callback rejects a request
// with no CSRF state cookie (it is the authorization gate).
func TestHandleCallback_MissingStateCookie(t *testing.T) {
	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("validcode", "s1", "")) // no cookie

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleCallback_StateMismatch verifies a query state that does not match
// the cookie is rejected.
func TestHandleCallback_StateMismatch(t *testing.T) {
	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("validcode", "query-state", "cookie-state"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleCallback_MissingCode verifies that, with a valid CSRF state, a
// missing code returns 400.
func TestHandleCallback_MissingCode(t *testing.T) {
	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: "https://github.test"})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("", "s1", "s1"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleCallback_ExchangeFailure verifies a bad GitHub response returns 502.
func TestHandleCallback_ExchangeFailure(t *testing.T) {
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(fakeGH.Close)

	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: fakeGH.URL})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("badcode", "s1", "s1"))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestHandleCallback_Success verifies the happy path: a valid CSRF state and
// code exchange stores credentials, calls mintFn, confirms registration, and
// NEVER echoes the raw setup token.
func TestHandleCallback_Success(t *testing.T) {
	fakeGH := fakeGitHubSuccess(t, 1001)

	mintCalled := false
	mintFn := func(installationID, _ string) (string, error) {
		mintCalled = true
		if installationID != "setup" {
			t.Errorf("mintFn installationID = %q, want setup", installationID)
		}
		return "raw-setup-token", nil
	}

	r, _, st := newManifestHandlerForTest(t, manifestTestOpts{githubBase: fakeGH.URL, mintFn: mintFn})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("validcode", "s1", "s1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !mintCalled {
		t.Error("mintFn should have been called")
	}
	body := w.Body.String()
	if !strings.Contains(body, "registered") {
		t.Error("response should confirm registration")
	}
	// The raw setup token must NOT leak through the GitHub redirect chain.
	if strings.Contains(body, "raw-setup-token") {
		t.Error("response body must not echo the raw setup token")
	}
	// Credentials should have been persisted.
	if _, ok, err := st.LoadAppCredentials(); err != nil || !ok {
		t.Errorf("credentials not saved: ok=%v err=%v", ok, err)
	}
}

// TestHandleCallback_SuccessNoMintFn verifies success with no mintFn still
// confirms registration.
func TestHandleCallback_SuccessNoMintFn(t *testing.T) {
	fakeGH := fakeGitHubSuccess(t, 2002)

	r, _, _ := newManifestHandlerForTest(t, manifestTestOpts{githubBase: fakeGH.URL})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("code2", "s1", "s1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "registered") {
		t.Error("response should confirm registration")
	}
}

// TestHandleCallback_RefusesOverwrite verifies the callback refuses to overwrite
// existing credentials when re-bootstrap is disabled, even with a valid state.
func TestHandleCallback_RefusesOverwrite(t *testing.T) {
	fakeGH := fakeGitHubSuccess(t, 3003)

	r, _, st := newManifestHandlerForTest(t, manifestTestOpts{githubBase: fakeGH.URL})
	seedCredentials(t, st)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, callbackReq("validcode", "s1", "s1"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}

// TestHtmlAttrEscape verifies that special HTML characters are escaped correctly.
func TestHtmlAttrEscape(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`<script>`, `&lt;script&gt;`},
		{`"quote"`, `&quot;quote&quot;`},
		{`a & b`, `a &amp; b`},
		{`plain`, `plain`},
		{`<"&>`, `&lt;&quot;&amp;&gt;`},
	}
	for _, tt := range tests {
		got := htmlAttrEscape(tt.in)
		if got != tt.want {
			t.Errorf("htmlAttrEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
