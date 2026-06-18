package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/store"
)

// fakeVerifier is a test double for the Verifier interface. It returns the
// configured Token (with installationID + optional userID/userLogin), ok and
// err regardless of the hash it is given — sufficient to exercise every
// branch of Required.
type fakeVerifier struct {
	installationID string
	userID         int64 // 0 = legacy / no user binding
	userLogin      string
	tokenID        string
	ok             bool
	err            error
}

func (f fakeVerifier) VerifyToken(_ string) (store.Token, bool, error) {
	tok := store.Token{
		ID:              f.tokenID,
		InstallationID:  f.installationID,
		GitHubUserLogin: f.userLogin,
	}
	if f.userID > 0 {
		v := f.userID
		tok.GitHubUserID = &v
	}
	return tok, f.ok, f.err
}

func TestGenerateToken(t *testing.T) {
	raw1, hash1, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken returned error: %v", err)
	}
	if raw1 == "" || hash1 == "" {
		t.Fatalf("GenerateToken returned empty values: raw=%q hash=%q", raw1, hash1)
	}
	if raw1 == hash1 {
		t.Fatalf("raw and hash should differ, both = %q", raw1)
	}

	// HashToken(raw) must reproduce the hash, and must be deterministic.
	if got := HashToken(raw1); got != hash1 {
		t.Fatalf("HashToken(raw) = %q, want %q", got, hash1)
	}
	if got := HashToken(raw1); got != hash1 {
		t.Fatalf("HashToken not deterministic: %q != %q", got, hash1)
	}

	// A hex SHA-256 is 64 lowercase hex chars.
	if len(hash1) != 64 {
		t.Fatalf("hash length = %d, want 64", len(hash1))
	}

	// Distinct invocations must yield distinct tokens.
	raw2, hash2, err := GenerateToken()
	if err != nil {
		t.Fatalf("second GenerateToken returned error: %v", err)
	}
	if raw1 == raw2 {
		t.Fatalf("two GenerateToken calls produced identical raw tokens: %q", raw1)
	}
	if hash1 == hash2 {
		t.Fatalf("two GenerateToken calls produced identical hashes: %q", hash1)
	}
}

func TestRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		wantID    = "inst-123"
		wantLogin = "octocat"
		wantUID   = int64(42)
	)

	type header struct {
		key, value string
	}

	tests := []struct {
		name        string
		verifier    Verifier
		headers     []header
		wantStatus  int
		wantBody    string // non-empty only when the next handler should run
		wantContext bool
		wantUserID  int64  // 0 for legacy / unauth
		wantLogin   string // "" for legacy / unauth
	}{
		{
			name:        "valid bearer token (user-bound)",
			verifier:    fakeVerifier{installationID: wantID, ok: true, userID: wantUID, userLogin: wantLogin},
			headers:     []header{{"Authorization", "Bearer some-token"}},
			wantStatus:  http.StatusOK,
			wantBody:    wantID,
			wantContext: true,
			wantUserID:  wantUID,
			wantLogin:   wantLogin,
		},
		{
			name:        "legacy token (no user binding) still authorized",
			verifier:    fakeVerifier{installationID: wantID, ok: true},
			headers:     []header{{"Authorization", "Bearer some-token"}},
			wantStatus:  http.StatusOK,
			wantBody:    wantID,
			wantContext: true,
			wantUserID:  0, // legacy sentinel
			wantLogin:   "",
		},
		{
			name:        "valid via X-Caw-Token header",
			verifier:    fakeVerifier{installationID: wantID, ok: true},
			headers:     []header{{"X-Caw-Token", "some-token"}},
			wantStatus:  http.StatusOK,
			wantBody:    wantID,
			wantContext: true,
		},
		{
			name:       "missing header",
			verifier:   fakeVerifier{installationID: wantID, ok: true},
			headers:    nil,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "empty bearer value falls through to missing",
			verifier:   fakeVerifier{installationID: wantID, ok: true},
			headers:    []header{{"Authorization", "Bearer "}},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown token",
			verifier:   fakeVerifier{ok: false},
			headers:    []header{{"Authorization", "Bearer some-token"}},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "verifier error",
			verifier:   fakeVerifier{err: errors.New("db down")},
			headers:    []header{{"Authorization", "Bearer some-token"}},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				sawContext  bool
				gotUserID   int64
				gotLogin    string
			)

			r := gin.New()
			r.GET("/pending", Required(tt.verifier), func(c *gin.Context) {
				id, exists := c.Get(ContextInstallationID)
				sawContext = exists
				if uid, ok := c.Get(ContextGitHubUserID); ok {
					gotUserID, _ = uid.(int64)
				}
				if l, ok := c.Get(ContextGitHubUserLogin); ok {
					gotLogin, _ = l.(string)
				}
				idStr, _ := id.(string)
				c.String(http.StatusOK, idStr)
			})

			req := httptest.NewRequest(http.MethodGet, "/pending", nil)
			for _, h := range tt.headers {
				req.Header.Set(h.key, h.value)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if sawContext != tt.wantContext {
				t.Fatalf("next-handler ran (context set) = %v, want %v", sawContext, tt.wantContext)
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Fatalf("body = %q, want %q", w.Body.String(), tt.wantBody)
			}
			if tt.wantContext {
				if gotUserID != tt.wantUserID {
					t.Fatalf("ContextGitHubUserID = %d, want %d", gotUserID, tt.wantUserID)
				}
				if gotLogin != tt.wantLogin {
					t.Fatalf("ContextGitHubUserLogin = %q, want %q", gotLogin, tt.wantLogin)
				}
			}
		})
	}
}

// TestGenerateID asserts the ULID-shaped id helper returns 26-char unique
// strings — exactly the width of the CHAR(26) tokens.id / auth_sessions.id
// columns. Two consecutive calls MUST differ (randomness covers the low 80
// bits even at the same millisecond).
func TestGenerateID(t *testing.T) {
	id1, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID: %v", err)
	}
	if got := len(id1); got != 26 {
		t.Fatalf("len(id) = %d, want 26", got)
	}
	id2, err := GenerateID()
	if err != nil {
		t.Fatalf("GenerateID #2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two GenerateID calls returned identical ids: %q", id1)
	}
}
