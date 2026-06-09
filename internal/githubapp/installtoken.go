package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// InstallationTokenClient fetches and caches GitHub installation access tokens.
// Tokens are valid for ~1 h; the cache refreshes when within 5 min of expiry.
type InstallationTokenClient struct {
	httpClient *http.Client
	baseURL    string
	signer     *AppJWTSigner

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

const refreshWindow = 5 * time.Minute

// NewInstallationTokenClient constructs a client bound to signer and baseURL.
// An empty baseURL defaults to api.github.com.
func NewInstallationTokenClient(signer *AppJWTSigner, baseURL string) *InstallationTokenClient {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &InstallationTokenClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    baseURL,
		signer:     signer,
		cache:      make(map[string]cachedToken),
	}
}

// Token returns a valid installation access token for installationID, using a
// cached value when it has more than refreshWindow left before expiry.
func (c *InstallationTokenClient) Token(ctx context.Context, installationID string) (string, error) {
	c.mu.Lock()
	if ct, ok := c.cache[installationID]; ok && time.Until(ct.expiresAt) > refreshWindow {
		tok := ct.token
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	tok, exp, err := c.fetch(ctx, installationID)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.cache[installationID] = cachedToken{token: tok, expiresAt: exp}
	c.mu.Unlock()

	return tok, nil
}

// installationTokenResponse is the GitHub API response body.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// fetch calls POST /app/installations/{id}/access_tokens using a fresh JWT.
func (c *InstallationTokenClient) fetch(ctx context.Context, installationID string) (string, time.Time, error) {
	now := time.Now()
	appJWT, err := c.signer.Sign(now, time.Time{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("installtoken: sign jwt: %w", err)
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", c.baseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("installtoken: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+appJWT)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("installtoken: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("installtoken: installation %s: status %d", installationID, resp.StatusCode)
	}

	var body installationTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", time.Time{}, fmt.Errorf("installtoken: decode: %w", err)
	}
	if body.Token == "" {
		return "", time.Time{}, fmt.Errorf("installtoken: empty token in response")
	}
	return body.Token, body.ExpiresAt, nil
}
