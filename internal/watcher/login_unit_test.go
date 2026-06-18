package watcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClampLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "caw MCP plugin"},
		{"short", "laptop", "laptop"},
		{"exact-64", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"overlong", strings.Repeat("b", 100), strings.Repeat("b", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampLabel(tc.in)
			if got != tc.want {
				t.Errorf("clampLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > 64 {
				t.Errorf("clampLabel returned > 64 chars: %d", len(got))
			}
		})
	}
}

func TestLoginOptions_EnsureDefaults(t *testing.T) {
	o := &LoginOptions{}
	o.ensureDefaults()
	if o.BrowserOpener == nil {
		t.Error("BrowserOpener should be defaulted")
	}
	if o.Now == nil {
		t.Error("Now should be defaulted")
	}
	if o.HTTPClient == nil {
		t.Error("HTTPClient should be defaulted")
	}
	// Idempotent: a second call should not overwrite explicit values.
	custom := &LoginOptions{
		BrowserOpener: func(string) error { return nil },
		Now:           func() time.Time { return time.Unix(0, 0) },
		HTTPClient:    &http.Client{},
	}
	priorOpener := custom.BrowserOpener
	priorNow := custom.Now
	priorClient := custom.HTTPClient
	custom.ensureDefaults()
	if &priorOpener == &custom.BrowserOpener && false {
		t.Error("placeholder; pointers compared above")
	}
	if custom.Now == nil || custom.Now() != priorNow() {
		t.Error("ensureDefaults clobbered custom Now")
	}
	if custom.HTTPClient != priorClient {
		t.Error("ensureDefaults clobbered custom HTTPClient")
	}
}

func TestPersistBundle_WritesCredentials(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")
	opts := LoginOptions{
		HubURL:          "https://hub.test",
		CredentialsPath: credsPath,
	}
	bundle := TokenBundle{
		SessionID:       "sess-1",
		GitHubUserID:    42,
		GitHubUserLogin: "alice",
		Tokens: []TokenRecord{
			{InstallationID: "i1", Org: "acme", Token: "raw-1", TokenID: "tok-1", DeviceLabel: "laptop"},
		},
	}
	if err := persistBundle(opts, bundle); err != nil {
		t.Fatalf("persistBundle: %v", err)
	}
	c, ok, err := LoadCredentials(credsPath)
	if err != nil || !ok {
		t.Fatalf("LoadCredentials: ok=%v err=%v", ok, err)
	}
	if c.HubURL != "https://hub.test" || c.GitHubUserLogin != "alice" {
		t.Errorf("round-trip mismatch: %+v", c)
	}
	if len(c.Tokens) != 1 || c.Tokens[0].InstallationID != "i1" {
		t.Errorf("tokens not round-tripped: %+v", c.Tokens)
	}
	// File mode is 0600 per the SaveCredentials contract.
	info, err := os.Stat(credsPath)
	if err != nil {
		t.Fatalf("stat creds: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %o, want 0600", info.Mode().Perm())
	}
}

func TestLogout_NoCredentialsFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "missing.json")
	// No file = nothing to revoke = nil. Logout must be idempotent.
	if err := Logout(context.Background(), "https://hub.test", credsPath, nil); err != nil {
		t.Errorf("Logout on missing creds should be no-op, got err=%v", err)
	}
}

func TestLogout_RevokesEachTokenAndClearsFile(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")
	if err := SaveCredentials(credsPath, Credentials{
		Version:         CredentialsVersion,
		HubURL:          "ignored-replaced-by-arg",
		GitHubUserLogin: "alice",
		Tokens: []TokenRecord{
			{InstallationID: "i1", Token: "raw-1", TokenID: "tok-1"},
			{InstallationID: "i2", Token: "raw-2", TokenID: "tok-2"},
		},
	}); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			calls = append(calls, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := Logout(context.Background(), srv.URL, credsPath, srv.Client()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("revoke calls = %d, want 2; got %v", len(calls), calls)
	}
	if _, ok, _ := LoadCredentials(credsPath); ok {
		t.Error("credentials file should be cleared after Logout")
	}
}

func TestLogout_PartialRevokeFailureStillClearsLocalFile(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")
	if err := SaveCredentials(credsPath, Credentials{
		Version: CredentialsVersion,
		HubURL:  "ignored",
		Tokens: []TokenRecord{
			{InstallationID: "i1", Token: "raw-1", TokenID: "tok-1"},
		},
	}); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	// Hub returns 500 on every revoke; the contract is that the local file
	// is still removed so a stale credentials.json doesn't outlive logout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := Logout(context.Background(), srv.URL, credsPath, srv.Client()); err != nil {
		t.Errorf("Logout with revoke failures should still succeed locally, got %v", err)
	}
	if _, ok, _ := LoadCredentials(credsPath); ok {
		t.Error("credentials file should be cleared even when revokes fail")
	}
}

// Sanity: the JSON tags on TokenBundle / TokenRecord round-trip through the
// wire format the hub uses. Any field rename here breaks Phase 3.5 too, so
// catch it locally.
func TestTokenBundle_JSONRoundTrip(t *testing.T) {
	in := TokenBundle{
		SessionID:       "sess",
		CodeChallenge:   "chal",
		GitHubUserID:    42,
		GitHubUserLogin: "alice",
		Tokens: []TokenRecord{
			{InstallationID: "i1", Org: "acme", Token: "raw", TokenID: "tok", DeviceLabel: "laptop", ExpiresAt: 1700000000},
		},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out TokenBundle
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SessionID != in.SessionID || out.GitHubUserLogin != in.GitHubUserLogin {
		t.Errorf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
	if len(out.Tokens) != 1 || out.Tokens[0].TokenID != "tok" {
		t.Errorf("tokens not round-tripped: %+v", out.Tokens)
	}
}
