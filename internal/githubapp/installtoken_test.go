package githubapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/githubapp"
)

func TestInstallationTokenClient_Token(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	signer, err := githubapp.NewAppJWTSigner("42", pemBytes)
	if err != nil {
		t.Fatalf("NewAppJWTSigner: %v", err)
	}

	futureExp := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	type serverResp struct {
		token     string
		expiresAt time.Time
		status    int
	}

	tests := []struct {
		name           string
		serverResp     serverResp
		installationID string
		wantErr        bool
	}{
		{
			name:           "happy path",
			installationID: "inst-1",
			serverResp: serverResp{
				token:     "ghs_abc123",
				expiresAt: futureExp,
				status:    http.StatusCreated,
			},
			wantErr: false,
		},
		{
			name:           "server error",
			installationID: "inst-2",
			serverResp: serverResp{
				status: http.StatusInternalServerError,
			},
			wantErr: true,
		},
		{
			name:           "empty token",
			installationID: "inst-3",
			serverResp: serverResp{
				token:     "",
				expiresAt: futureExp,
				status:    http.StatusCreated,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify the request uses Bearer auth.
				auth := r.Header.Get("Authorization")
				if auth == "" {
					t.Errorf("missing Authorization header")
				}

				if tc.serverResp.status != http.StatusCreated {
					w.WriteHeader(tc.serverResp.status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"token":      tc.serverResp.token,
					"expires_at": tc.serverResp.expiresAt.Format(time.RFC3339),
				})
			}))
			defer srv.Close()

			client := githubapp.NewInstallationTokenClient(signer, srv.URL)
			tok, err := client.Token(context.Background(), tc.installationID)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok != tc.serverResp.token {
				t.Errorf("token = %q; want %q", tok, tc.serverResp.token)
			}
		})
	}
}

func TestInstallationTokenClient_CacheHit(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	signer, err := githubapp.NewAppJWTSigner("42", pemBytes)
	if err != nil {
		t.Fatalf("NewAppJWTSigner: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "ghs_cached",
			"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	client := githubapp.NewInstallationTokenClient(signer, srv.URL)

	// First call: should hit the server.
	tok1, err := client.Token(context.Background(), "inst-cache")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second call: should return from cache (server call count stays at 1).
	tok2, err := client.Token(context.Background(), "inst-cache")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if tok1 != tok2 {
		t.Errorf("expected same cached token; got %q and %q", tok1, tok2)
	}
	if callCount != 1 {
		t.Errorf("server called %d times; expected 1 (cache hit on second call)", callCount)
	}
}
