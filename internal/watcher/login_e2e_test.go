package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/auth"
)

// ──────────────────────────────────────────────────────────────────────────
// postStartLoopback / postStartDevice / pollOnce coverage via httptest hub.
// ──────────────────────────────────────────────────────────────────────────

func TestPostStartLoopback_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/start" || r.Method != http.MethodPost {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"sess-1","verification_url":"https://hub/auth/u/sess-1","expires_at":1700000600}`))
	}))
	defer srv.Close()
	opts := LoginOptions{HubURL: srv.URL, HTTPClient: srv.Client()}
	opts.ensureDefaults()
	got, err := postStartLoopback(context.Background(), opts, "chal", "http://127.0.0.1:51111/cb")
	if err != nil {
		t.Fatalf("postStartLoopback: %v", err)
	}
	if got.SessionID != "sess-1" || got.VerificationURL == "" {
		t.Errorf("unexpected response: %+v", got)
	}
}

func TestPostStartLoopback_HubError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	opts := LoginOptions{HubURL: srv.URL, HTTPClient: srv.Client()}
	opts.ensureDefaults()
	_, err := postStartLoopback(context.Background(), opts, "chal", "http://127.0.0.1:51111/cb")
	if err == nil {
		t.Fatal("expected error on 500 hub response")
	}
}

func TestPostStartDevice_SuccessAndIntervalDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Interval omitted on purpose — postStartDevice must default to 5s.
		_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"WDJB-MJHT","verification_uri":"https://hub/auth/device","verification_uri_complete":"https://hub/auth/device?code=WDJB-MJHT","expires_at":1700000600}`))
	}))
	defer srv.Close()
	opts := LoginOptions{HubURL: srv.URL, HTTPClient: srv.Client()}
	opts.ensureDefaults()
	got, err := postStartDevice(context.Background(), opts, "chal")
	if err != nil {
		t.Fatalf("postStartDevice: %v", err)
	}
	if got.DeviceCode != "dev-1" || got.UserCode != "WDJB-MJHT" {
		t.Errorf("unexpected response: %+v", got)
	}
	if got.Interval != 5 {
		t.Errorf("Interval = %d, want 5 (default)", got.Interval)
	}
}

func TestPostStartDevice_MissingCodesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"","user_code":""}`))
	}))
	defer srv.Close()
	opts := LoginOptions{HubURL: srv.URL, HTTPClient: srv.Client()}
	opts.ensureDefaults()
	_, err := postStartDevice(context.Background(), opts, "chal")
	if err == nil {
		t.Fatal("expected error when hub returns empty codes")
	}
}

func TestPollOnce_StatusBranches(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus string
		wantErr    bool
	}{
		{"ok", 200, `{"session_id":"s","tokens":[{"installation_id":"i","org":"o","token":"raw","token_id":"tk"}]}`, "ok", false},
		{"authorization_pending", 400, `{"error":"authorization_pending"}`, "authorization_pending", false},
		{"slow_down", 400, `{"error":"slow_down"}`, "slow_down", false},
		{"expired_token", 410, `{"error":"expired_token"}`, "expired_token", false},
		{"access_denied", 403, `{"error":"access_denied"}`, "access_denied", false},
		{"unknown_5xx_propagates_error", 502, `gateway down`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			opts := LoginOptions{HubURL: srv.URL, HTTPClient: srv.Client()}
			opts.ensureDefaults()
			bundle, status, err := pollOnce(context.Background(), opts, "dev-1", "verifier")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want err, got status=%q bundle=%+v", status, bundle)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if tc.wantStatus == "ok" && len(bundle.Tokens) != 1 {
				t.Errorf("bundle missing tokens: %+v", bundle)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────
// LoginDevice end-to-end: simulated hub serves /auth/start + /auth/poll,
// poller eventually receives the bundle, persistBundle writes credentials.
// ──────────────────────────────────────────────────────────────────────────

func TestLoginDevice_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")

	var pollCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/start", func(w http.ResponseWriter, _ *http.Request) {
		// Interval=1 so the test doesn't wait 5s per poll.
		_, _ = fmt.Fprintf(w, `{"device_code":"dev-1","user_code":"WDJB-MJHT","verification_uri":"http://hub/auth/device","verification_uri_complete":"http://hub/auth/device?code=WDJB-MJHT","expires_at":%d,"interval":1}`, time.Now().Add(time.Minute).Unix())
	})
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&pollCount, 1)
		switch n {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"session_id":"sess-1","code_challenge":"chal","github_user_id":42,"github_user_login":"alice","tokens":[{"installation_id":"i1","org":"acme","token":"raw","token_id":"tok-1","device_label":"laptop"}]}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	opts := LoginOptions{
		HubURL:          srv.URL,
		ClientLabel:     "test-device",
		CredentialsPath: credsPath,
		HTTPClient:      srv.Client(),
		Now:             time.Now,
		BrowserOpener:   func(string) error { return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bundle, err := LoginDevice(ctx, opts)
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if bundle.GitHubUserLogin != "alice" || len(bundle.Tokens) != 1 {
		t.Errorf("unexpected bundle: %+v", bundle)
	}

	c, ok, err := LoadCredentials(credsPath)
	if err != nil || !ok {
		t.Fatalf("credentials not persisted: ok=%v err=%v", ok, err)
	}
	if c.GitHubUserLogin != "alice" || c.HubURL != srv.URL {
		t.Errorf("credentials wrong: %+v", c)
	}
	if atomic.LoadInt32(&pollCount) < 2 {
		t.Errorf("poll count = %d, want >= 2 (pending then ok)", pollCount)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// LoginLoopback end-to-end: simulated hub serves /auth/start; the injected
// BrowserOpener acts as the browser and POSTs the bundle to the loopback
// listener; LoginLoopback returns the bundle and persists it.
// ──────────────────────────────────────────────────────────────────────────

func TestLoginLoopback_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")

	// Capture the loopback redirect URL the watcher sends so the fake browser
	// knows where to POST the bundle.
	loopbackURLCh := make(chan string, 1)
	challengeCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/start", func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var req struct {
			LoopbackRedirect string `json:"loopback_redirect"`
			CodeChallenge    string `json:"code_challenge"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req)
		select {
		case loopbackURLCh <- req.LoopbackRedirect:
		default:
		}
		select {
		case challengeCh <- req.CodeChallenge:
		default:
		}
		_, _ = fmt.Fprintf(w, `{"session_id":"sess-1","verification_url":"http://hub/auth/u/sess-1","expires_at":%d}`, time.Now().Add(time.Minute).Unix())
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	browserOpener := func(_ string) error {
		// Simulate the user completing OAuth in the browser: hub POSTs the
		// bundle to the loopback listener. We do that here, asynchronously,
		// from the same goroutine the watcher spawned as the browser.
		go func() {
			loopback := <-loopbackURLCh
			challenge := <-challengeCh
			bundle := TokenBundle{
				SessionID:       "sess-1",
				CodeChallenge:   challenge,
				GitHubUserID:    42,
				GitHubUserLogin: "alice",
				Tokens: []TokenRecord{
					{InstallationID: "i1", Org: "acme", Token: "raw-1", TokenID: "tok-1", DeviceLabel: "test-device"},
				},
			}
			body, _ := json.Marshal(bundle)
			// Retry briefly: the loopback listener races with our POST.
			deadline := time.Now().Add(3 * time.Second)
			var lastErr error
			for time.Now().Before(deadline) {
				req, _ := http.NewRequest(http.MethodPost, loopback, nil)
				req.Body = io.NopCloser(bytesReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == 200 {
						return
					}
					lastErr = fmt.Errorf("loopback returned %d", resp.StatusCode)
				} else {
					lastErr = err
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Errorf("fake browser failed to deliver bundle: %v", lastErr)
		}()
		return nil
	}

	opts := LoginOptions{
		HubURL:          srv.URL,
		ClientLabel:     "loopback-test",
		CredentialsPath: credsPath,
		HTTPClient:      srv.Client(),
		BrowserOpener:   browserOpener,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bundle, err := LoginLoopback(ctx, opts)
	if err != nil {
		t.Fatalf("LoginLoopback: %v", err)
	}
	if bundle.GitHubUserLogin != "alice" || len(bundle.Tokens) != 1 {
		t.Errorf("unexpected bundle: %+v", bundle)
	}
	c, ok, err := LoadCredentials(credsPath)
	if err != nil || !ok {
		t.Fatalf("credentials not persisted: ok=%v err=%v", ok, err)
	}
	if c.GitHubUserLogin != "alice" {
		t.Errorf("credentials wrong: %+v", c)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Required-field guards for both Login* entry points.
// ──────────────────────────────────────────────────────────────────────────

func TestLoginLoopback_RequiresHubURL(t *testing.T) {
	_, err := LoginLoopback(context.Background(), LoginOptions{CredentialsPath: "/tmp/x"})
	if err == nil {
		t.Fatal("expected error when HubURL is empty")
	}
}

func TestLoginLoopback_RequiresCredentialsPath(t *testing.T) {
	_, err := LoginLoopback(context.Background(), LoginOptions{HubURL: "https://hub"})
	if err == nil {
		t.Fatal("expected error when CredentialsPath is empty")
	}
}

func TestLoginDevice_RequiresHubURL(t *testing.T) {
	_, err := LoginDevice(context.Background(), LoginOptions{CredentialsPath: "/tmp/x"})
	if err == nil {
		t.Fatal("expected error when HubURL is empty")
	}
}

// Sanity check that the PKCE imports actually work (the auth package is the
// dep — if the import wedges, every test fails on compile).
func TestPKCEPair_RoundTrip(t *testing.T) {
	v, ch, err := auth.GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if v == "" || ch == "" {
		t.Fatal("PKCE pair empty")
	}
	if err := auth.VerifyPKCE(v, ch, auth.PKCEMethod); err != nil {
		t.Fatalf("VerifyPKCE failed for round-trip pair: %v", err)
	}
}

// bytesReader is a tiny helper so the test doesn't need bytes.NewReader
// imported in the body of the inline goroutine.
func bytesReader(b []byte) io.Reader { return &readerBuf{b: b} }

type readerBuf struct {
	b []byte
	i int
}

func (r *readerBuf) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
