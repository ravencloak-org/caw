// Package hub — shared GitHub OAuth + user/installations helpers.
//
// Extracted from install_callback.go in Auth v2 Phase 3 so the Auth v2
// /auth/cb/github handler can reuse the same exchangeOAuthCode + listUser-
// Installations + fetchUser primitives without a second HTTP client, error
// shape, or response-body limit. install_callback.go is now a thin caller of
// these helpers (its old wrappers stayed in place but delegate here).
//
// The functions return go errors only; HTTP-status mapping is the caller's
// job (install_callback renders install_error.html, /auth/cb/github renders
// auth-flow-specific error pages).
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ghInstallation is one entry in GitHub's GET /user/installations response.
// We only model the fields the auth flow needs.
type ghInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"` // "User" | "Organization"
	} `json:"account"`
	AppSlug string `json:"app_slug"`
}

// ghUser mirrors the subset of GET /user we care about: the numeric id (stable
// across login renames) and login (display + lookup name).
type ghUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// exchangeOAuthCodeShared posts to /login/oauth/access_token and returns the
// user-to-server access token. clientID/clientSecret authenticate the App;
// code is the single-use OAuth code GitHub redirected back to us with.
//
// Body decode is bounded to 64 KiB to defang a malicious upstream that
// streams JSON forever.
func exchangeOAuthCodeShared(ctx context.Context, hc HTTPDoer, githubBase, clientID, clientSecret, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		githubBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build oauth request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, resp.Body, 512)
		return "", fmt.Errorf("oauth status %d", resp.StatusCode)
	}

	var body struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode oauth response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("oauth no access_token: %s %s", body.Error, body.ErrorDescription)
	}
	return body.AccessToken, nil
}

// listUserInstallations calls GET /user/installations with the user-to-server
// token and returns the parsed list. The legacy userOwnsInstallation in
// install_callback.go does its own filter on top of this.
func listUserInstallations(ctx context.Context, hc HTTPDoer, apiBase, userToken string) ([]ghInstallation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/user/installations", nil)
	if err != nil {
		return nil, fmt.Errorf("build installations request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+userToken)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("installations request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("installations status %d", resp.StatusCode)
	}

	var body struct {
		Installations []ghInstallation `json:"installations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode installations: %w", err)
	}
	return body.Installations, nil
}

// fetchUser calls GET /user with the user-to-server token and returns the
// authenticated user's stable id + login. Used by /auth/cb/github to
// associate the OAuth result with a github_user_id on the auth_sessions row.
func fetchUser(ctx context.Context, hc HTTPDoer, apiBase, userToken string) (ghUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/user", nil)
	if err != nil {
		return ghUser{}, fmt.Errorf("build /user request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+userToken)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := hc.Do(req)
	if err != nil {
		return ghUser{}, fmt.Errorf("/user request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ghUser{}, fmt.Errorf("/user status %d", resp.StatusCode)
	}

	var u ghUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&u); err != nil {
		return ghUser{}, fmt.Errorf("decode /user: %w", err)
	}
	if u.ID == 0 {
		return ghUser{}, fmt.Errorf("/user returned empty id")
	}
	return u, nil
}
