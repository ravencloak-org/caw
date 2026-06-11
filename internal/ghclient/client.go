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

// Client calls the GitHub REST API.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// New returns a Client. An empty baseURL defaults to api.github.com; an empty
// token sends unauthenticated requests.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    baseURL,
		token:      token,
	}
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PullState{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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
