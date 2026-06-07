package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakeVerifier is a test double for the Verifier interface. It returns its
// configured values regardless of the hash it is given, which is sufficient to
// exercise every branch of Required.
type fakeVerifier struct {
	installationID string
	ok             bool
	err            error
}

func (f fakeVerifier) VerifyToken(_ string) (string, bool, error) {
	return f.installationID, f.ok, f.err
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

	const wantID = "inst-123"

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
	}{
		{
			name:        "valid bearer token",
			verifier:    fakeVerifier{installationID: wantID, ok: true},
			headers:     []header{{"Authorization", "Bearer some-token"}},
			wantStatus:  http.StatusOK,
			wantBody:    wantID,
			wantContext: true,
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
			var sawContext bool

			r := gin.New()
			r.GET("/pending", Required(tt.verifier), func(c *gin.Context) {
				id, exists := c.Get(ContextInstallationID)
				sawContext = exists
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
		})
	}
}
