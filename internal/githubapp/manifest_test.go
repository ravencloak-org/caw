package githubapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ravencloak-org/caw/internal/githubapp"
)

// TestConvertManifest covers the GitHub POST /app-manifests/{code}/conversions call.
func TestConvertManifest(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		serverStatus int
		serverBody   map[string]interface{}
		wantErr      bool
	}{
		{
			name:         "happy path",
			code:         "abc123",
			serverStatus: http.StatusCreated,
			serverBody: map[string]interface{}{
				"id":             int64(12345),
				"client_id":      "Iv1.abc",
				"client_secret":  "client_secret_value",
				"webhook_secret": "webhook_secret_value",
				"pem":            "-----BEGIN RSA PRIVATE KEY-----\nfakepem\n-----END RSA PRIVATE KEY-----",
			},
			wantErr: false,
		},
		{
			name:         "server returns 404 (code expired)",
			code:         "bad_code",
			serverStatus: http.StatusNotFound,
			serverBody:   nil,
			wantErr:      true,
		},
		{
			name:         "server returns 201 with empty pem",
			code:         "no_pem",
			serverStatus: http.StatusCreated,
			serverBody: map[string]interface{}{
				"id":             int64(99),
				"client_id":      "Iv1.xyz",
				"client_secret":  "secret",
				"webhook_secret": "whsecret",
				"pem":            "",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify path and method.
				want := "/app-manifests/" + tc.code + "/conversions"
				if r.URL.Path != want {
					t.Errorf("path = %q; want %q", r.URL.Path, want)
				}
				if r.Method != http.MethodPost {
					t.Errorf("method = %q; want POST", r.Method)
				}
				w.WriteHeader(tc.serverStatus)
				if tc.serverBody != nil {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(tc.serverBody)
				}
			}))
			defer srv.Close()

			creds, err := githubapp.ConvertManifest(context.Background(), srv.URL, tc.code)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify fields from server response.
			body := tc.serverBody
			if wantID := body["id"].(int64); creds.AppID != wantID {
				t.Errorf("AppID = %d; want %d", creds.AppID, wantID)
			}
			if want := body["client_id"].(string); creds.ClientID != want {
				t.Errorf("ClientID = %q; want %q", creds.ClientID, want)
			}
			if want := body["client_secret"].(string); creds.ClientSecret != want {
				t.Errorf("ClientSecret = %q; want %q", creds.ClientSecret, want)
			}
			if want := body["webhook_secret"].(string); creds.WebhookSecret != want {
				t.Errorf("WebhookSecret = %q; want %q", creds.WebhookSecret, want)
			}
			if want := body["pem"].(string); creds.PEM != want {
				t.Errorf("PEM = %q; want %q", creds.PEM, want)
			}
		})
	}
}
