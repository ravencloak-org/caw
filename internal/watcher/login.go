// Package watcher — Auth v2 MCP `login` tool implementation (Phase 3).
//
// LoginLoopback opens a TCP listener on 127.0.0.1:0, posts (mode=loopback,
// PKCE challenge, listener port) to /auth/start, opens the browser to the
// returned verification_url, then accepts exactly one POST on the listener
// with the TokenBundle. PKCE binding: the hub echoes back the challenge it
// stored at /auth/start; the watcher verifies it matches sha256(verifier).
//
// LoginDevice runs the OAuth 2.0 device-code flow: posts (mode=device, PKCE
// challenge) to /auth/start, prints the user_code + verification_uri_complete
// to stderr + the MCP text channel, then polls /auth/poll until 200 +
// TokenBundle or a terminal error.
//
// Both paths converge on a TokenBundle, which Login* persists to the
// credentials file. The browser launcher is a soft dependency — a launcher
// failure prints the URL to stderr and the user opens it manually.
package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/ravencloak-org/caw/internal/auth"
)

// loginWaitTimeout is how long Login* waits for the user to finish OAuth
// before declaring the login canceled. Five minutes matches the plan's
// /auth/start session TTL minus the picker rendering time.
const loginWaitTimeout = 5 * time.Minute

// TokenBundle mirrors the hub's POST /cb payload (loopback) and /auth/poll
// response (device). Same shape, same field tags.
type TokenBundle struct {
	SessionID       string        `json:"session_id"`
	CodeChallenge   string        `json:"code_challenge"`
	GitHubUserID    int64         `json:"github_user_id"`
	GitHubUserLogin string        `json:"github_user_login"`
	Tokens          []TokenRecord `json:"tokens"`
	// Error is set by the hub on user_canceled / etc; non-empty means the
	// other fields may be missing.
	Error string `json:"error,omitempty"`
}

// LoginOptions configures Login. Hub-side parameters that the operator wants
// configurable per call go here; auth-flow internals (PKCE entropy, port
// retry count) stay constant.
type LoginOptions struct {
	// HubURL is the hub base URL. Required.
	HubURL string
	// ClientLabel is the human-readable device identifier (≤64 chars). Defaults
	// to the os.Hostname() value when empty.
	ClientLabel string
	// CredentialsPath is the destination file. Required.
	CredentialsPath string
	// BrowserOpener defaults to openBrowser; tests inject a no-op.
	BrowserOpener func(string) error
	// Now is the clock seam. Defaults to time.Now.
	Now func() time.Time
	// HTTPClient overrides the HTTP client used for /auth/start + /auth/poll.
	// Defaults to a 30s-timeout client.
	HTTPClient *http.Client
}

func (o *LoginOptions) ensureDefaults() {
	if o.BrowserOpener == nil {
		o.BrowserOpener = openBrowser
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
}

// startResponseLoopback / startResponseDevice mirror the hub /auth/start
// response shapes.
type startResponseLoopback struct {
	SessionID       string `json:"session_id"`
	VerificationURL string `json:"verification_url"`
	ExpiresAt       int64  `json:"expires_at"`
}

type startResponseDevice struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresAt               int64  `json:"expires_at"`
	Interval                int    `json:"interval"`
}

// LoginLoopback runs the loopback handshake against the hub at opts.HubURL.
// On success, persists the bundle to opts.CredentialsPath and returns it.
// The caller (the MCP tool handler) renders the user-facing acknowledgement.
func LoginLoopback(ctx context.Context, opts LoginOptions) (TokenBundle, error) {
	opts.ensureDefaults()
	if opts.HubURL == "" {
		return TokenBundle{}, fmt.Errorf("login: HubURL required")
	}
	if opts.CredentialsPath == "" {
		return TokenBundle{}, fmt.Errorf("login: CredentialsPath required")
	}

	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return TokenBundle{}, fmt.Errorf("login: PKCE generate: %w", err)
	}

	// Bind a 127.0.0.1 listener; OS picks the port. Try up to 5 times in case
	// of the rare race where another local process grabs the same port.
	var lis net.Listener
	for i := 0; i < 5; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			lis = l
			break
		}
	}
	if lis == nil {
		return TokenBundle{}, fmt.Errorf("login: bind loopback listener: all 5 attempts failed")
	}
	defer func() { _ = lis.Close() }()

	port := lis.Addr().(*net.TCPAddr).Port
	loopbackURL := fmt.Sprintf("http://127.0.0.1:%d/cb", port)

	startResp, err := postStartLoopback(ctx, opts, challenge, loopbackURL)
	if err != nil {
		return TokenBundle{}, err
	}

	log.Printf("verification URL: %s", startResp.VerificationURL)
	if err := opts.BrowserOpener(startResp.VerificationURL); err != nil {
		log.Printf("could not open browser (%v); please visit the URL above manually", err)
	}

	bundle, err := acceptLoopbackBundle(ctx, lis, startResp.SessionID, challenge, verifier)
	if err != nil {
		return TokenBundle{}, err
	}
	if bundle.Error != "" {
		return bundle, fmt.Errorf("login: %s", bundle.Error)
	}

	if err := persistBundle(opts, bundle); err != nil {
		return bundle, fmt.Errorf("login: persist credentials: %w", err)
	}
	return bundle, nil
}

// LoginDevice runs the device-code handshake against opts.HubURL. Same return
// shape as LoginLoopback. The MCP tool prints user_code + verification URL to
// the user via mcp.TextContent — Login* itself just returns the bundle.
func LoginDevice(ctx context.Context, opts LoginOptions) (TokenBundle, error) {
	opts.ensureDefaults()
	if opts.HubURL == "" {
		return TokenBundle{}, fmt.Errorf("login: HubURL required")
	}
	if opts.CredentialsPath == "" {
		return TokenBundle{}, fmt.Errorf("login: CredentialsPath required")
	}

	verifier, challenge, err := auth.GeneratePKCE()
	if err != nil {
		return TokenBundle{}, fmt.Errorf("login: PKCE generate: %w", err)
	}

	startResp, err := postStartDevice(ctx, opts, challenge)
	if err != nil {
		return TokenBundle{}, err
	}

	log.Printf("device login: open %s and enter code %s",
		startResp.VerificationURIComplete, startResp.UserCode)

	bundle, err := pollDevice(ctx, opts, startResp, verifier)
	if err != nil {
		return TokenBundle{}, err
	}

	if err := persistBundle(opts, bundle); err != nil {
		return bundle, fmt.Errorf("login: persist credentials: %w", err)
	}
	return bundle, nil
}

// postStartLoopback POSTs /auth/start with the loopback parameters and returns
// the decoded response.
func postStartLoopback(ctx context.Context, opts LoginOptions, challenge, loopbackURL string) (startResponseLoopback, error) {
	body, _ := json.Marshal(map[string]any{
		"mode":                  "loopback",
		"loopback_redirect":     loopbackURL,
		"code_challenge":        challenge,
		"code_challenge_method": auth.PKCEMethod,
		"client_label":          clampLabel(opts.ClientLabel),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(opts.HubURL, "/")+"/auth/start", bytes.NewReader(body))
	if err != nil {
		return startResponseLoopback{}, fmt.Errorf("login: build /auth/start: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return startResponseLoopback{}, fmt.Errorf("login: POST /auth/start: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return startResponseLoopback{}, fmt.Errorf("login: /auth/start status %d: %s", resp.StatusCode, b)
	}
	var sr startResponseLoopback
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return startResponseLoopback{}, fmt.Errorf("login: decode /auth/start response: %w", err)
	}
	return sr, nil
}

// postStartDevice is the device-flow counterpart to postStartLoopback.
func postStartDevice(ctx context.Context, opts LoginOptions, challenge string) (startResponseDevice, error) {
	body, _ := json.Marshal(map[string]any{
		"mode":                  "device",
		"code_challenge":        challenge,
		"code_challenge_method": auth.PKCEMethod,
		"client_label":          clampLabel(opts.ClientLabel),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(opts.HubURL, "/")+"/auth/start", bytes.NewReader(body))
	if err != nil {
		return startResponseDevice{}, fmt.Errorf("login: build /auth/start (device): %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return startResponseDevice{}, fmt.Errorf("login: POST /auth/start (device): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return startResponseDevice{}, fmt.Errorf("login: /auth/start (device) status %d: %s", resp.StatusCode, b)
	}
	var sr startResponseDevice
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return startResponseDevice{}, fmt.Errorf("login: decode /auth/start (device) response: %w", err)
	}
	if sr.DeviceCode == "" || sr.UserCode == "" {
		return startResponseDevice{}, fmt.Errorf("login: /auth/start (device) missing codes")
	}
	if sr.Interval <= 0 {
		sr.Interval = 5
	}
	return sr, nil
}

// acceptLoopbackBundle blocks on a one-shot HTTP server bound to lis,
// accepting exactly one POST /cb with the TokenBundle, verifying the PKCE
// challenge round-trip and the session id, then returning the bundle.
//
// On context cancellation or loginWaitTimeout, returns a "login canceled"
// error so the MCP tool surfaces a clear failure to the user.
func acceptLoopbackBundle(ctx context.Context, lis net.Listener, sessionID, challenge, verifier string) (TokenBundle, error) {
	type result struct {
		bundle TokenBundle
		err    error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/cb", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var b TokenBundle
		if err := json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&b); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("decode loopback bundle: %w", err)}
			return
		}
		// PKCE binding: the hub echoes back the challenge it stored at
		// /auth/start. We confirm sha256(verifier) matches both that echo
		// AND the challenge we generated locally — same value, two
		// independent paths to it.
		expectedChallenge := auth.S256Challenge(verifier)
		if b.CodeChallenge != expectedChallenge || b.CodeChallenge != challenge {
			http.Error(w, "challenge mismatch", http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("login: loopback challenge mismatch — possible interceptor")}
			return
		}
		if b.SessionID != sessionID {
			http.Error(w, "session mismatch", http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("login: loopback session_id mismatch")}
			return
		}
		w.WriteHeader(http.StatusNoContent)
		resultCh <- result{bundle: b}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(lis) }()

	waitCtx, cancel := context.WithTimeout(ctx, loginWaitTimeout)
	defer cancel()
	select {
	case r := <-resultCh:
		_ = srv.Shutdown(context.Background())
		return r.bundle, r.err
	case <-waitCtx.Done():
		_ = srv.Shutdown(context.Background())
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return TokenBundle{}, fmt.Errorf("login: timed out after %s — user did not complete the browser flow", loginWaitTimeout)
		}
		return TokenBundle{}, fmt.Errorf("login: canceled: %w", waitCtx.Err())
	}
}

// pollDevice loops POST /auth/poll with the device_code + code_verifier until
// the hub returns 200 (success), 403 (access_denied), 410 (expired_token), or
// the per-flow loginWaitTimeout fires.
func pollDevice(ctx context.Context, opts LoginOptions, start startResponseDevice, verifier string) (TokenBundle, error) {
	waitCtx, cancel := context.WithTimeout(ctx, loginWaitTimeout)
	defer cancel()
	interval := time.Duration(start.Interval) * time.Second
	for {
		select {
		case <-waitCtx.Done():
			return TokenBundle{}, fmt.Errorf("login: device-flow timeout after %s", loginWaitTimeout)
		case <-time.After(interval):
		}
		bundle, status, err := pollOnce(ctx, opts, start.DeviceCode, verifier)
		if err != nil {
			return TokenBundle{}, err
		}
		switch status {
		case "ok":
			return bundle, nil
		case "authorization_pending":
			continue
		case "slow_down":
			interval += time.Second
			continue
		case "expired_token":
			return TokenBundle{}, fmt.Errorf("login: device code expired — restart")
		case "access_denied":
			return TokenBundle{}, fmt.Errorf("login: access denied (user canceled or PKCE failed)")
		default:
			return TokenBundle{}, fmt.Errorf("login: unknown poll status %q", status)
		}
	}
}

// pollOnce performs one /auth/poll call and returns the bundle (on success)
// or one of the four documented error codes.
func pollOnce(ctx context.Context, opts LoginOptions, deviceCode, verifier string) (TokenBundle, string, error) {
	body, _ := json.Marshal(map[string]string{
		"device_code":   deviceCode,
		"code_verifier": verifier,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(opts.HubURL, "/")+"/auth/poll", bytes.NewReader(body))
	if err != nil {
		return TokenBundle{}, "", fmt.Errorf("login: build /auth/poll: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return TokenBundle{}, "", fmt.Errorf("login: POST /auth/poll: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var b TokenBundle
		if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&b); err != nil {
			return TokenBundle{}, "", fmt.Errorf("login: decode /auth/poll bundle: %w", err)
		}
		return b, "ok", nil
	case http.StatusBadRequest, http.StatusForbidden, http.StatusGone:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		return TokenBundle{}, e.Error, nil
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return TokenBundle{}, "", fmt.Errorf("login: /auth/poll status %d: %s", resp.StatusCode, b)
	}
}

// persistBundle writes the TokenBundle to credentials.json at opts.CredentialsPath.
func persistBundle(opts LoginOptions, b TokenBundle) error {
	return SaveCredentials(opts.CredentialsPath, Credentials{
		Version:         CredentialsVersion,
		HubURL:          opts.HubURL,
		GitHubUserID:    b.GitHubUserID,
		GitHubUserLogin: b.GitHubUserLogin,
		Tokens:          b.Tokens,
	})
}

// clampLabel enforces the hub's 64-char ClientLabel ceiling. An empty label
// falls back to "caw MCP plugin" so /auth/start never sees an empty field.
func clampLabel(label string) string {
	if label == "" {
		label = "caw MCP plugin"
	}
	if len(label) > 64 {
		return label[:64]
	}
	return label
}

// openBrowser is the platform-aware launcher. Failures here are non-fatal —
// the caller prints the URL to stderr so the user can open it manually.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// Logout revokes every token in the credentials file server-side, then clears
// the file. Errors revoking individual tokens are logged but don't block the
// local file removal — a stale credentials.json is worse than a leaked entry
// the operator can revoke from /me/tokens (Phase 4) later.
func Logout(ctx context.Context, hubURL, credentialsPath string, hc *http.Client) error {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	c, ok, err := LoadCredentials(credentialsPath)
	if err != nil {
		return fmt.Errorf("logout: load credentials: %w", err)
	}
	if !ok {
		return nil // nothing to do
	}
	for _, t := range c.Tokens {
		if t.TokenID == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
			strings.TrimRight(hubURL, "/")+"/me/tokens/"+t.TokenID, nil)
		if err != nil {
			log.Printf("logout: build DELETE for token %s: %v", t.TokenID, err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+t.Token)
		resp, err := hc.Do(req)
		if err != nil {
			log.Printf("logout: DELETE token %s: %v", t.TokenID, err)
			continue
		}
		_ = resp.Body.Close()
		// Belt-and-suspenders: a 404 here is treated as success.
		// /me/tokens/:id landed in Auth v2 Phase 4 and a freshly minted
		// token always exists on the hub, but a stale credentials.json
		// (operator revoked the token via `hub revoke-token` or the
		// /me/recover panic button, then the user runs `logout` locally)
		// will hit 404. Logout must always succeed locally — leaving a
		// stale credentials.json in place because the server doesn't know
		// the id is worse than the missed remote revoke.
		if resp.StatusCode >= 500 {
			log.Printf("logout: DELETE token %s returned %d", t.TokenID, resp.StatusCode)
		}
	}
	return ClearCredentials(credentialsPath)
}
