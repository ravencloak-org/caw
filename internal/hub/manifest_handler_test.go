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

// newManifestHandlerForTest constructs a ManifestHandler pointing to a fake
// GitHub server and wires it into a Gin engine for HTTP testing.
func newManifestHandlerForTest(t *testing.T, githubBase string, mintFn func(string, string) (string, error)) (*gin.Engine, *ManifestHandler) {
	t.Helper()
	st := newTestStore(t)
	mh, err := NewManifestHandler(ManifestConfig{
		BaseURL:    "http://hub.example.com",
		GithubBase: githubBase,
		Store:      st,
		MintFn:     mintFn,
	})
	if err != nil {
		t.Fatalf("NewManifestHandler: %v", err)
	}
	r := gin.New()
	r.GET("/github/app/manifest", mh.HandleManifest)
	r.GET("/github/app/callback", mh.HandleCallback)
	return r, mh
}

// TestNewManifestHandler_RequiresBaseURL verifies that a missing BaseURL is rejected.
func TestNewManifestHandler_RequiresBaseURL(t *testing.T) {
	st := newTestStore(t)
	_, err := NewManifestHandler(ManifestConfig{Store: st})
	if err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

// TestNewManifestHandler_RequiresStore verifies that a nil Store is rejected.
func TestNewManifestHandler_RequiresStore(t *testing.T) {
	_, err := NewManifestHandler(ManifestConfig{BaseURL: "http://hub.example.com"})
	if err == nil {
		t.Fatal("expected error for nil Store")
	}
}

// TestNewManifestHandler_DefaultGithubBase verifies that GithubBase defaults to
// https://github.com when not set.
func TestNewManifestHandler_DefaultGithubBase(t *testing.T) {
	st := newTestStore(t)
	mh, err := NewManifestHandler(ManifestConfig{
		BaseURL: "http://hub.example.com",
		Store:   st,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mh.githubBase != "https://github.com" {
		t.Errorf("githubBase = %q, want %q", mh.githubBase, "https://github.com")
	}
}

// TestHandleManifest_HTML verifies that the manifest endpoint returns an HTML
// page containing a form that POSTs to the GitHub apps/new URL.
func TestHandleManifest_HTML(t *testing.T) {
	r, _ := newManifestHandlerForTest(t, "https://github.test", nil)

	req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `action=`) {
		t.Error("response should contain a form action attribute")
	}
	if !strings.Contains(body, `name="manifest"`) {
		t.Error("response should contain a manifest hidden input")
	}
	// Manifest JSON should be embedded (check one stable field).
	if !strings.Contains(body, "installation") {
		t.Error("manifest JSON should include 'installation' event")
	}
}

// TestHandleManifest_ManifestJSON verifies the embedded manifest contains the
// expected fields (hook URL, redirect URL, default_events).
func TestHandleManifest_ManifestJSON(t *testing.T) {
	st := newTestStore(t)
	mh, err := NewManifestHandler(ManifestConfig{
		BaseURL: "http://hub.example.com",
		Store:   st,
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

// TestHandleCallback_MissingCode checks that a missing code query param returns 400.
func TestHandleCallback_MissingCode(t *testing.T) {
	r, _ := newManifestHandlerForTest(t, "https://github.test", nil)

	req := httptest.NewRequest(http.MethodGet, "/github/app/callback", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleCallback_ExchangeFailure checks that a bad GitHub response returns 502.
func TestHandleCallback_ExchangeFailure(t *testing.T) {
	// Fake GitHub that returns 422 for the conversion endpoint.
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(fakeGH.Close)

	r, _ := newManifestHandlerForTest(t, fakeGH.URL, nil)

	req := httptest.NewRequest(http.MethodGet, "/github/app/callback?code=badcode", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestHandleCallback_Success checks the happy path: code exchange succeeds,
// credentials are stored, and a setup token is shown in the response.
func TestHandleCallback_Success(t *testing.T) {
	// Fake GitHub that returns valid credentials.
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             int64(1001),
			"client_id":      "Iv1.abc",
			"client_secret":  "secret-cs",
			"webhook_secret": "secret-ws",
			"pem":            "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n",
		})
	}))
	t.Cleanup(fakeGH.Close)

	mintCalled := false
	mintFn := func(installationID, _ string) (string, error) {
		mintCalled = true
		if installationID != "setup" {
			t.Errorf("mintFn installationID = %q, want setup", installationID)
		}
		return "raw-setup-token", nil
	}

	r, _ := newManifestHandlerForTest(t, fakeGH.URL, mintFn)

	req := httptest.NewRequest(http.MethodGet, "/github/app/callback?code=validcode", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !mintCalled {
		t.Error("mintFn should have been called")
	}
	body := w.Body.String()
	if !strings.Contains(body, "raw-setup-token") {
		t.Error("response body should contain the raw setup token")
	}
	// Verify the token is NOT prefixed with "token:" or logged indicators.
	if strings.Contains(body, "token:") {
		t.Error("response body should not leak token prefix")
	}
}

// TestHandleCallback_SuccessNoMintFn checks that when no mintFn is set the
// response is still 200.
func TestHandleCallback_SuccessNoMintFn(t *testing.T) {
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             int64(2002),
			"client_id":      "Iv1.xyz",
			"client_secret":  "cs",
			"webhook_secret": "ws",
			"pem":            "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n",
		})
	}))
	t.Cleanup(fakeGH.Close)

	r, _ := newManifestHandlerForTest(t, fakeGH.URL, nil)

	req := httptest.NewRequest(http.MethodGet, "/github/app/callback?code=code2", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "registered") {
		t.Error("response should confirm registration")
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
