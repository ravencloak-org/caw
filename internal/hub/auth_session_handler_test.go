package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

// authHandlerSetup builds an AuthSessionHandler wired against a stub GitHub
// server. Returns the gin router, the handler (for whitebox-ish state checks),
// the store, the stub server, and a fakeGitHubAuth pointer the test can mutate
// per case.
type authHandlerSetup struct {
	r      *gin.Engine
	h      *AuthSessionHandler
	st     *store.Store
	gh     *httptest.Server
	ghStub *fakeGitHubAuth
	mint   *mintCall
	now    int64
}

func newAuthHandlerSetup(t *testing.T) *authHandlerSetup {
	t.Helper()
	st := newTestStore(t)
	stub := &fakeGitHubAuth{
		t:                       t,
		installationsAccountID:  139674548,
		installationsAccountLog: "ravencloak-org",
	}
	srv := httptest.NewServer(authStubHandler(t, stub))
	t.Cleanup(srv.Close)

	mint := &mintCall{}
	mintFn := func(installationID, org, deviceLabel string, userID int64, userLogin string) (string, string, error) {
		*mint = mintCall{installationID, org, deviceLabel, userID, userLogin}
		const raw = "raw-token-mock"
		_ = st.InsertTokenRow(store.Token{
			ID:              "tok-" + installationID,
			Hash:            auth.HashToken(raw + "-" + installationID),
			InstallationID:  installationID,
			Org:             org,
			DeviceLabel:     deviceLabel,
			GitHubUserID:    &userID,
			GitHubUserLogin: userLogin,
		})
		return raw + "-" + installationID, "tok-" + installationID, nil
	}
	credsFn := func() (string, string, bool, error) {
		return "Iv1.test-client", "secret", true, nil
	}
	now := int64(1717000000)
	h, err := NewAuthSessionHandler(AuthSessionHandlerConfig{
		BaseURL:    "http://hub.example.com",
		GithubBase: srv.URL,
		APIBase:    srv.URL,
		Store:      st,
		MintFn:     mintFn,
		CredsFn:    credsFn,
		AppSlugFn:  func() string { return "caw-test-slug" },
		Now:        func() time.Time { return time.Unix(now, 0) },
	})
	if err != nil {
		t.Fatalf("NewAuthSessionHandler: %v", err)
	}
	r := gin.New()
	r.POST("/auth/start", h.HandleStart)
	r.GET("/auth/u/:session_id", h.HandleBrowserStart)
	r.GET("/auth/cb/github", h.HandleGithubCallback)
	r.GET("/auth/picker/:session_id", h.HandlePickerGet)
	r.POST("/auth/picker/:session_id", h.HandlePickerPost)
	r.GET("/auth/device", h.HandleDevice)
	r.POST("/auth/poll", h.HandlePoll)
	r.GET("/auth/done/:session_id", h.HandleDone)
	r.GET("/auth/start-help", h.HandleStartHelp)
	return &authHandlerSetup{r: r, h: h, st: st, gh: srv, ghStub: stub, mint: mint, now: now}
}

// authStubHandler multiplexes the GitHub paths the auth handler needs:
// /login/oauth/access_token, /user, /user/installations, /login/oauth/authorize
// (the last is a redirect target; we just 200 it so HandleBrowserStart's
// redirect can be asserted on Location).
func authStubHandler(t *testing.T, f *fakeGitHubAuth) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := f.oauthBody
		if body == "" {
			body = `{"access_token":"user-token-abc","token_type":"bearer","scope":""}`
		}
		status := f.oauthStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    int64(12345),
			"login": "alice",
		})
	})
	mux.HandleFunc("/user/installations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type acc struct {
			Login string `json:"login"`
			ID    int64  `json:"id"`
			Type  string `json:"type"`
		}
		type inst struct {
			ID      int64 `json:"id"`
			Account acc   `json:"account"`
		}
		var insts []inst
		if f.installationsAccountID != 0 {
			insts = append(insts, inst{
				ID:      f.installationsAccountID,
				Account: acc{Login: f.installationsAccountLog, ID: 1, Type: "Organization"},
			})
		}
		for _, id := range f.installationsExtraIDs {
			insts = append(insts, inst{ID: id, Account: acc{Login: "other-org", ID: id, Type: "Organization"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"installations": insts})
	})
	mux.HandleFunc("/login/oauth/authorize", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// startSession is a test helper that POSTs /auth/start and returns the
// session_id from the response.
func startSession(t *testing.T, s *authHandlerSetup, mode, challenge, loopback string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"mode":                  mode,
		"loopback_redirect":     loopback,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"client_label":          "test-client",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /auth/start status %d: %s", w.Code, w.Body.String())
	}
	if mode == "loopback" {
		var resp startLoopbackResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.SessionID
	}
	var resp startDeviceResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.DeviceCode
}

func TestAuthStart_LoopbackHappyPath(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")
	if sid == "" {
		t.Fatal("empty session_id returned")
	}
	row, ok, _ := s.st.GetAuthSession(sid)
	if !ok {
		t.Fatal("session not persisted")
	}
	if row.HandshakeMode != "loopback" {
		t.Errorf("HandshakeMode = %q", row.HandshakeMode)
	}
	if row.State != "pending" {
		t.Errorf("State = %q, want pending", row.State)
	}
}

func TestAuthStart_RejectsBadMode(t *testing.T) {
	s := newAuthHandlerSetup(t)
	body, _ := json.Marshal(map[string]any{
		"mode":                  "magic",
		"code_challenge":        "AAAA",
		"code_challenge_method": "S256",
		"client_label":          "x",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAuthStart_RejectsBadLoopbackRedirect(t *testing.T) {
	s := newAuthHandlerSetup(t)
	body, _ := json.Marshal(map[string]any{
		"mode":                  "loopback",
		"loopback_redirect":     "https://evil.example.com/cb",
		"code_challenge":        "AAAA",
		"code_challenge_method": "S256",
		"client_label":          "x",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAuthStart_DeviceHappyPath(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	body, _ := json.Marshal(map[string]any{
		"mode":                  "device",
		"code_challenge":        ch,
		"code_challenge_method": "S256",
		"client_label":          "test-client",
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp startDeviceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		t.Errorf("missing device/user code: %+v", resp)
	}
	if !strings.Contains(resp.VerificationURIComplete, resp.UserCode) {
		t.Errorf("verification_uri_complete missing user_code: %s", resp.VerificationURIComplete)
	}
	if resp.Interval != 5 {
		t.Errorf("interval = %d, want 5", resp.Interval)
	}
}

func TestAuthBrowserStart_SetsCookieAndRedirects(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")
	req := httptest.NewRequest(http.MethodGet, "/auth/u/"+sid, nil)
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/login/oauth/authorize") {
		t.Errorf("Location = %q, expected GitHub authorize URL", loc)
	}
	if !strings.Contains(loc, "state="+sid) {
		t.Errorf("Location missing state=%s", sid)
	}
	// Cookie set with the session id, HttpOnly, scoped to /auth.
	cookies := w.Result().Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == authSessionCookieName {
			found = true
			if c.Value != sid {
				t.Errorf("cookie value = %q, want %s", c.Value, sid)
			}
			if !c.HttpOnly {
				t.Errorf("cookie not HttpOnly")
			}
			if c.Path != "/auth" {
				t.Errorf("cookie path = %q, want /auth", c.Path)
			}
		}
	}
	if !found {
		t.Errorf("session cookie not set")
	}
}

func TestAuthBrowserStart_RejectsExpiredSession(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")
	// Advance the clock past the session TTL.
	s.h.now = func() time.Time { return time.Unix(s.now+3600, 0) }
	req := httptest.NewRequest(http.MethodGet, "/auth/u/"+sid, nil)
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want 410", w.Code)
	}
}

func TestAuthGithubCallback_StateMismatchRejected(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")
	req := httptest.NewRequest(http.MethodGet, "/auth/cb/github?state=tampered&code=x", nil)
	req.AddCookie(&http.Cookie{Name: authSessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for CSRF", w.Code)
	}
}

func TestAuthGithubCallback_HappyPathRedirectsToPicker(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")
	req := httptest.NewRequest(http.MethodGet, "/auth/cb/github?state="+sid+"&code=ghc-code", nil)
	req.AddCookie(&http.Cookie{Name: authSessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/auth/picker/"+sid {
		t.Errorf("Location = %q, want /auth/picker/%s", loc, sid)
	}
	// Session row updated.
	row, _, _ := s.st.GetAuthSession(sid)
	if row.State != "awaiting_picker" {
		t.Errorf("State = %q, want awaiting_picker", row.State)
	}
	if row.GitHubUserID == nil || *row.GitHubUserID != 12345 {
		t.Errorf("GitHubUserID = %v, want 12345", row.GitHubUserID)
	}
	if row.GitHubUserLogin != "alice" {
		t.Errorf("GitHubUserLogin = %q, want alice", row.GitHubUserLogin)
	}
}

func TestAuthGithubCallback_ZeroInstallsRedirectsToInstall(t *testing.T) {
	s := newAuthHandlerSetup(t)
	s.ghStub.installationsAccountID = 0 // empty list
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")
	req := httptest.NewRequest(http.MethodGet, "/auth/cb/github?state="+sid+"&code=ghc-code", nil)
	req.AddCookie(&http.Cookie{Name: authSessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/apps/caw-test-slug/installations/new") {
		t.Errorf("Location = %q, expected install redirect", loc)
	}
	if !strings.Contains(loc, "state="+sid) {
		t.Errorf("install redirect missing state=%s: %s", sid, loc)
	}
	row, _, _ := s.st.GetAuthSession(sid)
	if row.State != "awaiting_install" {
		t.Errorf("State = %q, want awaiting_install", row.State)
	}
}

func TestAuthPicker_MintsTokenAndFiresLoopback(t *testing.T) {
	s := newAuthHandlerSetup(t)

	// Stand up a loopback listener that captures the POST.
	var captured atomic.Value // map[string]any
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		captured.Store(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer loopback.Close()

	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, loopback.URL)

	// Walk through the OAuth callback so the session is in awaiting_picker.
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/cb/github?state="+sid+"&code=c", nil)
	cbReq.AddCookie(&http.Cookie{Name: authSessionCookieName, Value: sid})
	s.r.ServeHTTP(httptest.NewRecorder(), cbReq)

	// Picker GET — renders HTML
	getReq := httptest.NewRequest(http.MethodGet, "/auth/picker/"+sid, nil)
	getW := httptest.NewRecorder()
	s.r.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("picker GET status = %d: %s", getW.Code, getW.Body.String())
	}
	if !strings.Contains(getW.Body.String(), "ravencloak-org") {
		t.Errorf("picker body missing installation login")
	}

	// Picker POST — submit with one installation selected
	form := strings.NewReader("device_label=Test+Laptop&installation_ids%5B%5D=" + strconv.FormatInt(s.ghStub.installationsAccountID, 10))
	postReq := httptest.NewRequest(http.MethodPost, "/auth/picker/"+sid, form)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postW := httptest.NewRecorder()
	s.r.ServeHTTP(postW, postReq)
	if postW.Code != http.StatusOK {
		t.Fatalf("picker POST status = %d: %s", postW.Code, postW.Body.String())
	}

	// Session is now delivered.
	row, _, _ := s.st.GetAuthSession(sid)
	if row.State != "delivered" {
		t.Errorf("State = %q, want delivered", row.State)
	}

	// Mint was called with the right shape.
	if s.mint.userID != 12345 {
		t.Errorf("mint userID = %d, want 12345", s.mint.userID)
	}
	if s.mint.userLogin != "alice" {
		t.Errorf("mint userLogin = %q, want alice", s.mint.userLogin)
	}
	if s.mint.deviceLabel != "Test Laptop" {
		t.Errorf("mint deviceLabel = %q", s.mint.deviceLabel)
	}

	// Loopback POST landed; bundle echoes the challenge we generated.
	// Wait briefly for the goroutine to land (handler fires from request ctx).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if captured.Load() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	v := captured.Load()
	if v == nil {
		t.Fatalf("loopback POST never landed")
	}
	bundle := v.(map[string]any)
	if bundle["code_challenge"] != ch {
		t.Errorf("loopback bundle challenge mismatch: got %v, want %s", bundle["code_challenge"], ch)
	}
	if bundle["github_user_login"] != "alice" {
		t.Errorf("loopback bundle user_login = %v", bundle["github_user_login"])
	}
}

func TestAuthPicker_CancelFiresErrorLoopback(t *testing.T) {
	s := newAuthHandlerSetup(t)
	var captured atomic.Value
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		captured.Store(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer loopback.Close()

	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, loopback.URL)
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/cb/github?state="+sid+"&code=c", nil)
	cbReq.AddCookie(&http.Cookie{Name: authSessionCookieName, Value: sid})
	s.r.ServeHTTP(httptest.NewRecorder(), cbReq)

	form := strings.NewReader("cancel=1")
	req := httptest.NewRequest(http.MethodPost, "/auth/picker/"+sid, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)

	row, _, _ := s.st.GetAuthSession(sid)
	if row.State != "canceled" {
		t.Errorf("State = %q, want canceled", row.State)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if captured.Load() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	v := captured.Load()
	if v == nil {
		t.Fatalf("cancel loopback POST never landed")
	}
	if v.(map[string]any)["error"] != "user_canceled" {
		t.Errorf("loopback error = %v, want user_canceled", v.(map[string]any)["error"])
	}
}

func TestAuthPoll_PendingThenDelivered(t *testing.T) {
	s := newAuthHandlerSetup(t)
	v, ch, _ := auth.GeneratePKCE()
	deviceCode := startSession(t, s, "device", ch, "")

	// First poll: authorization_pending.
	pollBody, _ := json.Marshal(map[string]string{
		"device_code":   deviceCode,
		"code_verifier": v,
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/poll", bytes.NewReader(pollBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("first poll status = %d, want 400 pending", w.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "authorization_pending" {
		t.Errorf("first poll error = %v, want authorization_pending", resp["error"])
	}

	// Simulate the picker happy path: set the bundle on the session.
	row, _, _ := s.st.GetSessionByDeviceCode(deviceCode)
	bundle := `{"github_user_id":42,"tokens":[{"installation_id":"1","token":"raw"}]}`
	if err := s.st.SetSessionPendingBundle(row.ID, bundle); err != nil {
		t.Fatalf("SetSessionPendingBundle: %v", err)
	}

	// Sleep past the slow_down window before re-polling.
	time.Sleep(authPollSlowDownDelta + 100*time.Millisecond)

	req2 := httptest.NewRequest(http.MethodPost, "/auth/poll", bytes.NewReader(pollBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("delivered poll status = %d, body=%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `"github_user_id":42`) {
		t.Errorf("delivered body = %s", w2.Body.String())
	}
}

func TestAuthPoll_SlowDownEnforced(t *testing.T) {
	s := newAuthHandlerSetup(t)
	v, ch, _ := auth.GeneratePKCE()
	deviceCode := startSession(t, s, "device", ch, "")
	pollBody, _ := json.Marshal(map[string]string{
		"device_code":   deviceCode,
		"code_verifier": v,
	})
	doPoll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/poll", bytes.NewReader(pollBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.r.ServeHTTP(w, req)
		return w
	}
	w1 := doPoll()
	if w1.Code != http.StatusBadRequest {
		t.Fatalf("first poll status = %d", w1.Code)
	}
	w2 := doPoll() // immediately again — must be slow_down
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("second poll status = %d", w2.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["error"] != "slow_down" {
		t.Errorf("second poll error = %v, want slow_down", resp["error"])
	}
}

func TestAuthPoll_BadCodeVerifierAccessDenied(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	deviceCode := startSession(t, s, "device", ch, "")
	body, _ := json.Marshal(map[string]string{
		"device_code":   deviceCode,
		"code_verifier": strings.Repeat("a", 43), // wrong verifier
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/poll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestAuthPoll_ExpiredTokenForUnknownDeviceCode(t *testing.T) {
	s := newAuthHandlerSetup(t)
	body, _ := json.Marshal(map[string]string{
		"device_code":   "GHOST",
		"code_verifier": strings.Repeat("a", 43),
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/poll", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want 410 expired", w.Code)
	}
}

func TestAuthDone_PendingThenDelivered(t *testing.T) {
	s := newAuthHandlerSetup(t)
	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, "http://127.0.0.1:54711/cb")

	req := httptest.NewRequest(http.MethodGet, "/auth/done/"+sid, nil)
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Errorf("pre-delivery status = %d, want 202", w.Code)
	}

	// Mark delivered.
	if err := s.st.SetSessionPendingBundle(sid, `{}`); err != nil {
		t.Fatalf("SetSessionPendingBundle: %v", err)
	}
	w = httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("post-delivery status = %d, want 200", w.Code)
	}
}

func TestValidateLoopbackRedirect(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"http://127.0.0.1:54711/cb", false},
		{"http://localhost:8080/cb", false},
		{"http://[::1]:8080/cb", false},
		{"https://127.0.0.1:54711/cb", true}, // must be http
		{"http://evil.example.com/cb", true}, // not loopback
		{"http://127.0.0.1/cb", true},        // missing port
		{"", true},                           // empty
		{":::bad:::", true},                  // unparseable
	}
	for _, tc := range cases {
		err := validateLoopbackRedirect(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateLoopbackRedirect(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

// TestAuthFlow_LoopbackHappyPath drives /auth/start + /auth/cb/github +
// /auth/picker end-to-end. Issue #59's first acceptance bullet by name.
func TestAuthFlow_LoopbackHappyPath(t *testing.T) {
	s := newAuthHandlerSetup(t)
	var captured atomic.Value
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		captured.Store(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer loopback.Close()

	_, ch, _ := auth.GeneratePKCE()
	sid := startSession(t, s, "loopback", ch, loopback.URL)

	cb := httptest.NewRequest(http.MethodGet, "/auth/cb/github?state="+sid+"&code=c", nil)
	cb.AddCookie(&http.Cookie{Name: authSessionCookieName, Value: sid})
	s.r.ServeHTTP(httptest.NewRecorder(), cb)

	form := strings.NewReader("device_label=My+MBP&installation_ids%5B%5D=" + fmt.Sprintf("%d", s.ghStub.installationsAccountID))
	post := httptest.NewRequest(http.MethodPost, "/auth/picker/"+sid, form)
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postW := httptest.NewRecorder()
	s.r.ServeHTTP(postW, post)
	if postW.Code != http.StatusOK {
		t.Fatalf("picker POST status = %d: %s", postW.Code, postW.Body.String())
	}

	// Token issued through this path MUST carry github_user_id IS NOT NULL.
	tokens, _ := s.st.ListTokensForUser(12345)
	if len(tokens) == 0 {
		t.Fatal("no tokens found for user 12345")
	}
	if tokens[0].GitHubUserID == nil || *tokens[0].GitHubUserID != 12345 {
		t.Errorf("token GitHubUserID = %v", tokens[0].GitHubUserID)
	}
	if tokens[0].DeviceLabel != "My MBP" {
		t.Errorf("device_label = %q, want 'My MBP'", tokens[0].DeviceLabel)
	}
}

// TestAuthFlow_DevicePolling exercises authorization_pending → delivered.
// Issue #59's second acceptance bullet by name.
func TestAuthFlow_DevicePolling(t *testing.T) {
	s := newAuthHandlerSetup(t)
	v, ch, _ := auth.GeneratePKCE()
	deviceCode := startSession(t, s, "device", ch, "")
	pollBody, _ := json.Marshal(map[string]string{
		"device_code": deviceCode, "code_verifier": v,
	})
	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/poll", bytes.NewReader(pollBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.r.ServeHTTP(w, req)
		return w
	}
	// pending
	if w := poll(); w.Code != http.StatusBadRequest {
		t.Fatalf("first poll = %d, want pending", w.Code)
	}
	// simulate picker → bundle delivered
	row, _, _ := s.st.GetSessionByDeviceCode(deviceCode)
	_ = s.st.SetSessionPendingBundle(row.ID, `{"github_user_id":12345,"tokens":[]}`)

	time.Sleep(authPollSlowDownDelta + 200*time.Millisecond)
	w := poll()
	if w.Code != http.StatusOK {
		t.Fatalf("delivered poll = %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuthStartHelp_RendersHTML(t *testing.T) {
	s := newAuthHandlerSetup(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/start-help", nil)
	w := httptest.NewRecorder()
	s.r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
	if !strings.HasPrefix(strings.TrimSpace(w.Body.String()), "<!DOCTYPE html>") {
		t.Errorf("missing HTML doctype: %s", w.Body.String()[:80])
	}
	if !strings.Contains(w.Body.String(), "login") {
		t.Errorf("body missing login keyword")
	}
}

// suppress unused-import warning on io
var _ = io.LimitReader

// ── HandleDevice + renderDeviceWithError ─────────────────────────────────
// The /auth/device entry point is the human-facing form the user lands on
// after the CLI prints their user_code. These tests cover the four branches:
// blank form (no code), unknown code, expired code, valid → 302.

func TestAuthDevice_BlankFormWhenNoCode(t *testing.T) {
	s := newAuthHandlerSetup(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/device", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rr.Header().Get("Cache-Control"))
	}
	// Form rendered with empty PrefilledCode + no ErrorMessage — body should
	// contain the form skeleton but no error styling.
	body := rr.Body.String()
	if !strings.Contains(body, "<form") {
		t.Errorf("body missing <form: %s", body[:min(len(body), 200)])
	}
}

func TestAuthDevice_UnknownCodeRendersError(t *testing.T) {
	s := newAuthHandlerSetup(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/device?code=WDJB-MJHT", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Unknown code") {
		t.Errorf("body missing 'Unknown code' error: %s", body[:min(len(body), 400)])
	}
	// The PrefilledCode should be echoed back upper-cased so the user can
	// see what was looked up.
	if !strings.Contains(body, "WDJB-MJHT") {
		t.Errorf("body missing prefilled code: %s", body[:min(len(body), 400)])
	}
}

func TestAuthDevice_ExpiredCodeRendersError(t *testing.T) {
	s := newAuthHandlerSetup(t)
	// Insert an auth_session with a user_code that's already past its expiry.
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "expired-1",
		HandshakeMode:       "device",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		DeviceCode:          "dev-expired",
		UserCode:            "EXPI-RED1",
		CreatedAt:           now.Add(-20 * time.Minute).Unix(),
		ExpiresAt:           now.Add(-1 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/device?code=EXPI-RED1", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "expired") {
		t.Errorf("body missing 'expired' error: %s", body[:min(len(body), 400)])
	}
}

func TestAuthDevice_ValidCodeRedirectsToGithub(t *testing.T) {
	s := newAuthHandlerSetup(t)
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "live-1",
		HandshakeMode:       "device",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		DeviceCode:          "dev-live",
		UserCode:            "WDJB-MJHT",
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	// Lower-case input: handler normalizes to upper-case before lookup.
	req := httptest.NewRequest(http.MethodGet, "/auth/device?code=wdjb-mjht", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 redirect; body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/login/oauth/authorize") {
		t.Errorf("Location = %q, want /login/oauth/authorize redirect", loc)
	}
	// Cookie must carry the session id so /auth/cb/github can pick it up.
	gotCookie := rr.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range gotCookie {
		if c.Name == authSessionCookieName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("missing %s cookie", authSessionCookieName)
	}
	if sessionCookie.Value != "live-1" {
		t.Errorf("cookie value = %q, want live-1", sessionCookie.Value)
	}
}

func TestAuthDevice_CredsMissingReturns424(t *testing.T) {
	s := newAuthHandlerSetup(t)
	// Swap the creds source to one that says "not configured".
	s.h.cfg.CredsFn = func() (string, string, bool, error) {
		return "", "", false, nil
	}
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "no-creds-1",
		HandshakeMode:       "device",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		DeviceCode:          "dev-nc",
		UserCode:            "NOCR-EDS1",
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/device?code=NOCR-EDS1", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusFailedDependency {
		t.Errorf("status = %d, want 424 FailedDependency; body=%s", rr.Code, rr.Body.String())
	}
}
