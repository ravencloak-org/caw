// Package hub — GitHub App install Setup URL callback (ADR-0010).
package hub

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed install_callback.html
var installCallbackHTML string

// InstallCallbackHandler serves the GitHub App Setup URL callback that mints
// and one-time-displays a Watcher token after a user installs the App on a
// repository (ADR-0010 — self-service Watcher token issuance).
//
// Flow:
//
//  1. User installs the caw GitHub App on a repo via the GitHub UI.
//  2. GitHub redirects the user to setup_url with installation_id, setup_action,
//     and code (the latter only if "Request user authorization (OAuth) during
//     installation" is enabled in the App settings — required for self-service).
//  3. This handler exchanges code for a user-to-server OAuth token, then calls
//     GET /user/installations to verify the authenticated user actually owns
//     (i.e. has admin access to) installation_id.
//  4. On success, mints a fresh Watcher token via the same mint function the
//     CLI subcommand uses, and renders an HTML page that displays the token
//     exactly once. Refreshing the page returns "code already used" because
//     GitHub's OAuth codes are single-use.
//
// Same token shape as ADR-0003; only the issuance channel is new.
type InstallCallbackHandler struct {
	baseURL    string
	githubBase string
	apiBase    string
	credsFn    func() (clientID, clientSecret string, ok bool, err error)
	mintFn     MintFunc
	httpClient HTTPDoer
	tmpl       *template.Template
}

// HTTPDoer is satisfied by *http.Client and is the seam tests use to stub
// GitHub's OAuth and REST endpoints.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// InstallCallbackConfig is the configuration for an InstallCallbackHandler.
type InstallCallbackConfig struct {
	// BaseURL is this Hub's public URL, used in rendered config snippets.
	BaseURL string
	// GithubBase is github.com (configurable for tests); defaults to https://github.com.
	GithubBase string
	// APIBase is api.github.com (configurable for tests); defaults to https://api.github.com.
	APIBase string
	// CredsFn resolves the App's OAuth client_id/client_secret at REQUEST time.
	// It is called on every install-callback request, so an operator who later
	// runs the manifest flow (or seeds env vars on the next deploy) does not
	// need to restart for the handler to see fresh credentials.
	//
	// ok=false signals "no credentials available right now" → handler returns
	// 424 FailedDependency. err is reserved for genuine lookup failures.
	//
	// main.go's wiring stacks env (CAW_APP_CLIENT_ID/SECRET) over
	// store.LoadAppCredentials() so a hand-registered App that lives in env
	// works alongside a manifest-registered App that lives in the DB.
	CredsFn func() (clientID, clientSecret string, ok bool, err error)
	// MintFn issues a Watcher token bound to (installationID, org, deviceLabel,
	// userID, userLogin). It is the same function the `hub mint-token` CLI
	// subcommand and the installation.created webhook auto-mint path use.
	// Returns the raw token (shown once) and the persisted token id (CHAR(26)
	// ULID; for revoke / list / audit). Phase 1 still calls it with userID=0
	// from the install-callback path (legacy semantics, no user binding); Phase
	// 3 begins issuing user-bound tokens through the /auth/* surface.
	MintFn MintFunc
	// HTTPClient overrides the HTTP client used for GitHub OAuth + REST calls.
	// Defaults to http.DefaultClient.
	HTTPClient HTTPDoer
}

// MintFunc is the signature for the Hub's token mint function. It is shared by
// every issuance path (CLI, manifest callback, install callback, webhook
// auto-mint, Auth v2 /auth/picker handler in Phase 3+).
type MintFunc func(installationID, org, deviceLabel string, userID int64, userLogin string) (rawToken string, tokenID string, err error)

// NewInstallCallbackHandler constructs an InstallCallbackHandler from cfg.
func NewInstallCallbackHandler(cfg InstallCallbackConfig) (*InstallCallbackHandler, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("NewInstallCallbackHandler: BaseURL is required")
	}
	if cfg.CredsFn == nil {
		return nil, fmt.Errorf("NewInstallCallbackHandler: CredsFn is required")
	}
	if cfg.MintFn == nil {
		return nil, fmt.Errorf("NewInstallCallbackHandler: MintFn is required")
	}
	gh := cfg.GithubBase
	if gh == "" {
		gh = "https://github.com"
	}
	api := cfg.APIBase
	if api == "" {
		api = "https://api.github.com"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	tmpl, err := template.New("install_callback").Parse(installCallbackHTML)
	if err != nil {
		return nil, fmt.Errorf("NewInstallCallbackHandler parse template: %w", err)
	}
	return &InstallCallbackHandler{
		baseURL:    cfg.BaseURL,
		githubBase: gh,
		apiBase:    api,
		credsFn:    cfg.CredsFn,
		mintFn:     cfg.MintFn,
		httpClient: hc,
		tmpl:       tmpl,
	}, nil
}

// Handle serves GET /github/app/install/callback.
func (h *InstallCallbackHandler) Handle(c *gin.Context) {
	installID := c.Query("installation_id")
	if installID == "" {
		c.String(http.StatusBadRequest, "missing installation_id")
		return
	}
	if c.Query("setup_action") != "install" {
		c.String(http.StatusBadRequest, "unexpected setup_action; expected 'install'")
		return
	}
	code := c.Query("code")
	if code == "" {
		c.String(http.StatusBadRequest,
			`missing OAuth code: enable "Request user authorization (OAuth) during installation" in this App's GitHub settings, then reinstall`)
		return
	}

	clientID, clientSecret, ok, err := h.credsFn()
	if err != nil {
		log.Printf("install callback: load credentials: %v", err)
		c.String(http.StatusInternalServerError, "credentials lookup failed")
		return
	}
	if !ok || clientID == "" || clientSecret == "" {
		c.String(http.StatusFailedDependency,
			"App OAuth credentials not configured; set CAW_APP_CLIENT_ID/CAW_APP_CLIENT_SECRET or run the manifest flow")
		return
	}

	ctx := c.Request.Context()
	userToken, err := h.exchangeOAuthCode(ctx, clientID, clientSecret, code)
	if err != nil {
		log.Printf("install callback: oauth exchange: %v", err)
		c.String(http.StatusBadGateway, "OAuth exchange failed")
		return
	}

	accountLogin, ok, err := h.userOwnsInstallation(ctx, userToken, installID)
	if err != nil {
		log.Printf("install callback: user/installations: %v", err)
		c.String(http.StatusBadGateway, "GitHub API failed")
		return
	}
	if !ok {
		c.String(http.StatusForbidden, "you do not have admin access to this installation")
		return
	}

	// Phase 1 keeps the install-callback path on legacy semantics: no user
	// binding (userID=0) and DeviceLabel="legacy", so VerifyToken returns the
	// row with GitHubUserID == nil and Phase 2's RequireRepoAccess bypasses
	// it. Phase 3's /auth/picker handler is the path that mints user-bound
	// tokens through the same MintFunc.
	rawToken, _, err := h.mintFn(installID, accountLogin, "legacy", 0, "")
	if err != nil {
		log.Printf("install callback: mint: %v", err)
		c.String(http.StatusInternalServerError, "mint failed")
		return
	}

	// Response headers — token must never be cached, referer-leaked, or
	// surfaced via inline-script injection. Style + script tags need
	// 'unsafe-inline' for the embedded one-page template.
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Security-Policy",
		"default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'none'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	c.Header("Content-Type", "text/html; charset=utf-8")

	data := struct {
		Token     string
		HubURL    string
		Account   string
		InstallID string
	}{
		Token:     rawToken,
		HubURL:    h.baseURL,
		Account:   accountLogin,
		InstallID: installID,
	}
	if err := h.tmpl.Execute(c.Writer, data); err != nil {
		// Headers are already written; best we can do is log.
		log.Printf("install callback: render template: %v", err)
	}
}

// exchangeOAuthCode posts to GitHub's OAuth /access_token endpoint and returns
// the user-to-server token.
func (h *InstallCallbackHandler) exchangeOAuthCode(ctx context.Context, clientID, clientSecret, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.githubBase+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build oauth request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain a small chunk for debugging without echoing secrets.
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

// userOwnsInstallation calls GET /user/installations with userToken and returns
// the installation's account login if installID is in the list (ok=true).
func (h *InstallCallbackHandler) userOwnsInstallation(ctx context.Context, userToken, installID string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.apiBase+"/user/installations", nil)
	if err != nil {
		return "", false, fmt.Errorf("build installations request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+userToken)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("installations request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("installations status %d", resp.StatusCode)
	}

	var body struct {
		Installations []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		} `json:"installations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&body); err != nil {
		return "", false, fmt.Errorf("decode installations: %w", err)
	}

	for _, inst := range body.Installations {
		if fmt.Sprintf("%d", inst.ID) == installID {
			return inst.Account.Login, true, nil
		}
	}
	return "", false, nil
}
