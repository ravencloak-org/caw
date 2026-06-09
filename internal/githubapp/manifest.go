package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// AppCredentials holds all secrets returned by the GitHub App Manifest conversion flow.
type AppCredentials struct {
	AppID         int64
	ClientID      string
	ClientSecret  string
	WebhookSecret string
	PEM           string
}

// manifestConversionResponse mirrors the GitHub POST /app-manifests/{code}/conversions response.
type manifestConversionResponse struct {
	ID            int64  `json:"id"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// ConvertManifest exchanges a temporary GitHub App manifest code for full App credentials.
// It calls POST {baseURL}/app-manifests/{code}/conversions and expects HTTP 201.
func ConvertManifest(ctx context.Context, baseURL, code string) (AppCredentials, error) {
	if code == "" {
		return AppCredentials{}, fmt.Errorf("convertmanifest: empty code")
	}

	cl := &http.Client{Timeout: 15 * time.Second}
	url := fmt.Sprintf("%s/app-manifests/%s/conversions", baseURL, code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("convertmanifest: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := cl.Do(req)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("convertmanifest: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return AppCredentials{}, fmt.Errorf("convertmanifest: unexpected status %d", resp.StatusCode)
	}

	var body manifestConversionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return AppCredentials{}, fmt.Errorf("convertmanifest: decode: %w", err)
	}
	if body.PEM == "" {
		return AppCredentials{}, fmt.Errorf("convertmanifest: empty pem in response")
	}

	return AppCredentials{
		AppID:         body.ID,
		ClientID:      body.ClientID,
		ClientSecret:  body.ClientSecret,
		WebhookSecret: body.WebhookSecret,
		PEM:           body.PEM,
	}, nil
}
