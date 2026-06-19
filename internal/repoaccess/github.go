package repoaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// InstallTokenSource resolves a GitHub App installation access token for an
// installation id. The real implementation is
// (*internal/githubapp.InstallationTokenClient).Token; tests inject a
// closure that returns a fixed string.
type InstallTokenSource func(ctx context.Context, installationID string) (string, error)

// httpChecker implements Checker by calling
// GET /repos/{owner}/{repo}/collaborators/{username}/permission with a fresh
// installation token for installationID. We do NOT store user OAuth tokens —
// the App installation already grants the read needed to answer the question.
type httpChecker struct {
	apiBase    string
	installTok InstallTokenSource
	httpClient *http.Client
}

// DefaultHTTPTimeout caps a single permission check at a value short enough
// that a hung GitHub call cannot pin an SSE-bound goroutine for more than a
// few seconds. The middleware enforces fail-closed on timeout via
// ErrUnavailable so a slow GitHub never silently allows.
const DefaultHTTPTimeout = 10 * time.Second

// NewHTTPChecker constructs a Checker that talks to GitHub's REST API.
//   - apiBase: "" defaults to https://api.github.com.
//   - installTok: required — the function used to mint a per-installation
//     access token on each check (cached upstream by InstallationTokenClient).
//   - hc: nil defaults to a fresh http.Client with DefaultHTTPTimeout.
func NewHTTPChecker(apiBase string, installTok InstallTokenSource, hc *http.Client) Checker {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	return &httpChecker{apiBase: apiBase, installTok: installTok, httpClient: hc}
}

// permissionResponse is the subset of the GitHub response we care about.
// The endpoint returns shape:
//
//	{ "permission": "admin"|"maintain"|"write"|"triage"|"read"|"none",
//	  "user": { ... } }
type permissionResponse struct {
	Permission string `json:"permission"`
}

// HasReadAccess implements Checker. It maps the GitHub status codes onto the
// repoaccess decision model:
//
//	200 + permission != "none" → allow
//	200 + permission == "none" → deny (rare: user is a collaborator with read removed)
//	404                        → deny (user is not a collaborator)
//	403                        → ErrConfigError (the App lacks the right scope)
//	5xx / network / timeout    → ErrUnavailable
//	any other status           → ErrUnavailable (defensive)
func (c *httpChecker) HasReadAccess(ctx context.Context, installationID, userLogin, owner, repo string) (bool, error) {
	if c.installTok == nil {
		return false, fmt.Errorf("%w: no install token source", ErrUnavailable)
	}
	tok, err := c.installTok(ctx, installationID)
	if err != nil {
		return false, fmt.Errorf("%w: install token: %v", ErrUnavailable, err)
	}
	if tok == "" {
		return false, fmt.Errorf("%w: empty install token", ErrUnavailable)
	}

	endpoint := fmt.Sprintf(
		"%s/repos/%s/%s/collaborators/%s/permission",
		c.apiBase,
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(userLogin),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		var body permissionResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false, fmt.Errorf("%w: decode body: %v", ErrUnavailable, err)
		}
		return body.Permission != "" && body.Permission != "none", nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode == http.StatusForbidden:
		return false, fmt.Errorf("%w: status %d (check App permissions: members read)", ErrConfigError, resp.StatusCode)
	case resp.StatusCode >= 500:
		return false, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	default:
		return false, fmt.Errorf("%w: unexpected status %d", ErrUnavailable, resp.StatusCode)
	}
}
