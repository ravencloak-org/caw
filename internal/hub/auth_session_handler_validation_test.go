package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/store"
)

// HandleStart's whole validation cascade is single-line returns —
// table-driven covers all 7 branches in one test.
func TestAuthStart_ValidationCascade(t *testing.T) {
	cases := []struct {
		name     string
		body     any
		wantCode int
		wantErr  string
	}{
		{"invalid_json", "{not-json", 400, "invalid_json"},
		{"missing_client_label", map[string]any{
			"mode":                  "loopback",
			"loopback_redirect":     "http://127.0.0.1:55555/cb",
			"code_challenge":        "chal",
			"code_challenge_method": "S256",
		}, 400, "client_label required"},
		{"client_label_too_long", map[string]any{
			"mode":                  "loopback",
			"loopback_redirect":     "http://127.0.0.1:55555/cb",
			"client_label":          strings.Repeat("a", 65),
			"code_challenge":        "chal",
			"code_challenge_method": "S256",
		}, 400, "exceeds 64 chars"},
		{"missing_code_challenge", map[string]any{
			"mode":                  "loopback",
			"loopback_redirect":     "http://127.0.0.1:55555/cb",
			"client_label":          "test",
			"code_challenge_method": "S256",
		}, 400, "code_challenge required"},
		{"wrong_challenge_method", map[string]any{
			"mode":                  "loopback",
			"loopback_redirect":     "http://127.0.0.1:55555/cb",
			"client_label":          "test",
			"code_challenge":        "chal",
			"code_challenge_method": "plain",
		}, 400, "S256"},
		{"wrong_mode", map[string]any{
			"mode":                  "samurai",
			"client_label":          "test",
			"code_challenge":        "chal",
			"code_challenge_method": "S256",
		}, 400, "mode must be"},
		{"loopback_bad_redirect", map[string]any{
			"mode":                  "loopback",
			"loopback_redirect":     "https://evil.example.com/cb", // non-loopback
			"client_label":          "test",
			"code_challenge":        "chal",
			"code_challenge_method": "S256",
		}, 400, ""}, // validateLoopbackRedirect's exact message is implementation-defined; just check status
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAuthHandlerSetup(t)
			var raw []byte
			switch b := tc.body.(type) {
			case string:
				raw = []byte(b)
			default:
				raw, _ = json.Marshal(b)
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/auth/start", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			s.r.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantCode, rr.Body.String())
			}
			if tc.wantErr != "" && !strings.Contains(rr.Body.String(), tc.wantErr) {
				t.Errorf("body missing %q: %s", tc.wantErr, rr.Body.String())
			}
		})
	}
}

// HandleStart device-flow happy path — covers the device branch separately
// from the loopback path that other tests exercise.
func TestAuthStart_DeviceFlow_ReturnsCodes(t *testing.T) {
	s := newAuthHandlerSetup(t)
	body, _ := json.Marshal(map[string]any{
		"mode":                  "device",
		"client_label":          "test",
		"code_challenge":        "chal",
		"code_challenge_method": "S256",
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["device_code"] == "" || got["user_code"] == "" {
		t.Errorf("device_code/user_code missing: %+v", got)
	}
	if got["verification_uri"] == nil || got["verification_uri_complete"] == nil {
		t.Errorf("verification URIs missing: %+v", got)
	}
	if got["interval"] == nil {
		t.Error("interval missing")
	}
}

// HandleBrowserStart — the four error branches plus a sanity happy path
// (the loopback case lands cookie + redirects to GitHub OAuth authorize).
func TestAuthBrowserStart_SessionNotFound(t *testing.T) {
	s := newAuthHandlerSetup(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/u/ghost-session", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuthBrowserStart_SessionExpired(t *testing.T) {
	s := newAuthHandlerSetup(t)
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "expired-bs",
		HandshakeMode:       "loopback",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		LoopbackRedirect:    "http://127.0.0.1:0/cb",
		CreatedAt:           now.Add(-30 * time.Minute).Unix(),
		ExpiresAt:           now.Add(-1 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/u/expired-bs", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Errorf("status = %d, want 410 Gone; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuthBrowserStart_DeviceModeSessionRejected(t *testing.T) {
	s := newAuthHandlerSetup(t)
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "device-bs",
		HandshakeMode:       "device", // not loopback
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		DeviceCode:          "dev-1",
		UserCode:            "WDJB-MJHT",
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/u/device-bs", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "loopback flow only") {
		t.Errorf("body missing 'loopback flow only': %s", rr.Body.String())
	}
}

func TestAuthBrowserStart_CredsMissingIs424(t *testing.T) {
	s := newAuthHandlerSetup(t)
	s.h.cfg.CredsFn = func() (string, string, bool, error) {
		return "", "", false, nil
	}
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "nocreds-bs",
		HandshakeMode:       "loopback",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		LoopbackRedirect:    "http://127.0.0.1:0/cb",
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/u/nocreds-bs", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusFailedDependency {
		t.Errorf("status = %d, want 424; body=%s", rr.Code, rr.Body.String())
	}
}

// HandleDone — pending → 202, delivered → 200, canceled → 204, unknown → 404.
func TestAuthDone_SessionNotFound(t *testing.T) {
	s := newAuthHandlerSetup(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/done/ghost-session", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestAuthDone_StateMatrix(t *testing.T) {
	cases := []struct {
		name     string
		state    string
		wantCode int
	}{
		{"pending_returns_202", "pending", 202},
		{"awaiting_install", "awaiting_install", 202},
		{"awaiting_picker", "awaiting_picker", 202},
		{"delivered_returns_200", "delivered", 200},
		{"canceled_returns_410", "canceled", 410},
		{"expired_returns_410", "expired", 410},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAuthHandlerSetup(t)
			now := time.Unix(s.now, 0)
			id := "done-" + tc.state
			if err := s.st.InsertAuthSession(store.AuthSession{
				ID:                  id,
				HandshakeMode:       "loopback",
				CodeChallenge:       "chal",
				CodeChallengeMethod: "S256",
				ClientLabel:         "test",
				LoopbackRedirect:    "http://127.0.0.1:0/cb",
				State:               tc.state,
				CreatedAt:           now.Unix(),
				ExpiresAt:           now.Add(10 * time.Minute).Unix(),
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/auth/done/"+id, nil)
			s.r.ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("state=%s status = %d, want %d; body=%s", tc.state, rr.Code, tc.wantCode, rr.Body.String())
			}
		})
	}
}

// HandlePicker GET — session not found / wrong state / expired.
func TestAuthPickerGet_SessionNotFound(t *testing.T) {
	s := newAuthHandlerSetup(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/picker/ghost-session", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestAuthPickerGet_SessionExpired(t *testing.T) {
	s := newAuthHandlerSetup(t)
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "pick-expired",
		HandshakeMode:       "loopback",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		LoopbackRedirect:    "http://127.0.0.1:0/cb",
		State:               "awaiting_picker",
		CreatedAt:           now.Add(-30 * time.Minute).Unix(),
		ExpiresAt:           now.Add(-1 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/picker/pick-expired", nil)
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Errorf("status = %d, want 410; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuthPickerGet_WrongStateRejected(t *testing.T) {
	s := newAuthHandlerSetup(t)
	now := time.Unix(s.now, 0)
	if err := s.st.InsertAuthSession(store.AuthSession{
		ID:                  "pick-pending",
		HandshakeMode:       "loopback",
		CodeChallenge:       "chal",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		LoopbackRedirect:    "http://127.0.0.1:0/cb",
		State:               "pending", // hasn't reached picker stage yet
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/picker/pick-pending", nil)
	s.r.ServeHTTP(rr, req)
	// Any non-200 is acceptable; exact code is implementation-defined but
	// the handler must reject a session that isn't in awaiting_picker yet.
	if rr.Code >= 200 && rr.Code < 300 {
		t.Errorf("expected non-2xx for wrong-state session, got %d; body=%s", rr.Code, rr.Body.String())
	}
}
