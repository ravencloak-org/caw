// Package ghclient is a minimal GitHub REST client for the Hub's outbound
// calls (the mergeability poll — the one poll in the system). It is a plain
// net/http client; Gin is the inbound server framework and does not apply here.
package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// TokenSource resolves the bearer token for a given repository, letting the
// client authenticate with a GitHub App installation token (scoped per repo)
// or a static PAT. A nil TokenSource sends unauthenticated requests.
type TokenSource func(ctx context.Context, owner, repo string) (string, error)

// StaticToken returns a TokenSource that always yields tok regardless of repo
// (a personal access token). An empty tok sends unauthenticated requests.
func StaticToken(tok string) TokenSource {
	return func(context.Context, string, string) (string, error) { return tok, nil }
}

// Client calls the GitHub REST API.
type Client struct {
	httpClient *http.Client
	baseURL    string
	tokenSrc   TokenSource
}

// New returns a Client. An empty baseURL defaults to api.github.com; a nil
// tokenSrc sends unauthenticated requests.
func New(baseURL string, tokenSrc TokenSource) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    baseURL,
		tokenSrc:   tokenSrc,
	}
}

// token resolves the bearer token for owner/repo, or "" when no source is set.
func (c *Client) token(ctx context.Context, owner, repo string) (string, error) {
	if c.tokenSrc == nil {
		return "", nil
	}
	return c.tokenSrc(ctx, owner, repo)
}

// PullState is the subset of GET /pulls/{n} the Hub uses for mergeability.
type PullState struct {
	State          string `json:"state"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
}

// EnableAutoMerge enables the GitHub auto-merge feature for a pull request using
// the merge method (defaults to "merge" if empty). This causes GitHub to merge
// the PR automatically once all required status checks pass (ADR-0002 Slice 6).
func (c *Client) EnableAutoMerge(ctx context.Context, owner, repo string, number int, mergeMethod string) error {
	if mergeMethod == "" {
		mergeMethod = "merge"
	}
	tok, err := c.token(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("enable auto-merge: token: %w", err)
	}
	// GitHub auto-merge is enabled via PATCH /repos/{owner}/{repo}/pulls/{pull_number}
	// with { "auto_merge": { "merge_method": "..." } }.
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)

	body := fmt.Sprintf(`{"auto_merge":{"merge_method":%q}}`, mergeMethod)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url,
		strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("enable auto-merge: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("enable auto-merge: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enable auto-merge %s/%s#%d: status %d", owner, repo, number, resp.StatusCode)
	}
	return nil
}

// PullMergeability fetches a PR's mergeability.
func (c *Client) PullMergeability(ctx context.Context, owner, repo string, number int) (PullState, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)
	tok, err := c.token(ctx, owner, repo)
	if err != nil {
		return PullState{}, fmt.Errorf("mergeability token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PullState{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PullState{}, fmt.Errorf("get pull: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return PullState{}, fmt.Errorf("get pull %s/%s#%d: status %d", owner, repo, number, resp.StatusCode)
	}

	var ps PullState
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return PullState{}, fmt.Errorf("decode pull: %w", err)
	}
	return ps, nil
}
