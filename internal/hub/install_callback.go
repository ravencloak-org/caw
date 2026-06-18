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

//go:embed install_error.html
var installErrorHTML string

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
	mintFn     func(installationID, org string) (string, error)
	httpClient HTTPDoer
	tmpl       *template.Template
	errorTmpl  *template.Template
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
	// MintFn issues a Watcher token bound to (installationID, org); same function the
	// `hub mint-token` CLI subcommand uses, returning the raw token (shown once).
	MintFn func(installationID, org string) (string, error)
	// HTTPClient overrides the HTTP client used for GitHub OAuth + REST calls.
	// Defaults to http.DefaultClient.
	HTTPClient HTTPDoer
}

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
	errorTmpl, err := template.New("install_error").Parse(installErrorHTML)
	if err != nil {
		return nil, fmt.Errorf("NewInstallCallbackHandler parse error template: %w", err)
	}
	return &InstallCallbackHandler{
		baseURL:    cfg.BaseURL,
		githubBase: gh,
		apiBase:    api,
		credsFn:    cfg.CredsFn,
		mintFn:     cfg.MintFn,
		httpClient: hc,
		tmpl:       tmpl,
		errorTmpl:  errorTmpl,
	}, nil
}

// Handle serves GET /github/app/install/callback.
//
// GitHub redirects users here with one of two setup_action values:
//   - "install" — fresh install; exchange OAuth code, verify the user, mint a token.
//   - "update"  — installation was reconfigured (e.g. repo added/removed); no new
//     token is minted, browser lands on a soft-redirect page pointing at /me/tokens
//     (which lands in Phase 4 of Auth v2 — for now the link 404s and we tell the
//     user how to rotate via `hub mint-token`).
//
// Every failure path renders install_error.html with actionable copy + a Restart
// login button, instead of a bare-text response. The `code` strings ("missing_oauth_code",
// "oauth_exchange_failed", etc.) are stable and used both for log correlation and as
// the lookup key into installErrorCopy for title + next-step bullets.
func (h *InstallCallbackHandler) Handle(c *gin.Context) {
	installID := c.Query("installation_id")
	if installID == "" {
		h.renderError(c, http.StatusBadRequest, "missing_installation_id",
			"GitHub redirected here without an installation_id query parameter.",
			h.restartURL())
		return
	}
	switch c.Query("setup_action") {
	case "install":
		// fall through to the OAuth + mint flow below.
	case "update":
		h.renderSetupActionUpdate(c)
		return
	default:
		h.renderError(c, http.StatusBadRequest, "unsupported_setup_action",
			"GitHub sent setup_action="+c.Query("setup_action")+", but this endpoint only handles install and update.",
			h.restartURL())
		return
	}
	code := c.Query("code")
	if code == "" {
		h.renderError(c, http.StatusBadRequest, "missing_oauth_code",
			`GitHub didn't include an OAuth code. The App's "Request user authorization (OAuth) during installation" checkbox is probably off.`,
			h.restartURL())
		return
	}

	clientID, clientSecret, ok, err := h.credsFn()
	if err != nil {
		log.Printf("install callback: load credentials: %v", err)
		h.renderError(c, http.StatusInternalServerError, "creds_lookup_failed",
			"Hub couldn't load its App credentials from configuration or storage.",
			"")
		return
	}
	if !ok || clientID == "" || clientSecret == "" {
		h.renderError(c, http.StatusFailedDependency, "no_credentials",
			"App OAuth credentials are not configured on this hub.",
			"")
		return
	}

	ctx := c.Request.Context()
	userToken, err := h.exchangeOAuthCode(ctx, clientID, clientSecret, code)
	if err != nil {
		log.Printf("install callback: oauth exchange: %v", err)
		h.renderError(c, http.StatusBadGateway, "oauth_exchange_failed",
			"GitHub refused to exchange the OAuth code for an access token — the code may have expired or been reused.",
			h.restartURL())
		return
	}

	accountLogin, ok, err := h.userOwnsInstallation(ctx, userToken, installID)
	if err != nil {
		log.Printf("install callback: user/installations: %v", err)
		h.renderError(c, http.StatusBadGateway, "installations_lookup_failed",
			"Hub couldn't list your GitHub installations after the OAuth exchange — likely a transient GitHub API failure.",
			h.restartURL())
		return
	}
	if !ok {
		h.renderError(c, http.StatusForbidden, "not_an_admin",
			"Your GitHub account isn't an admin of installation "+installID+" — only an admin can mint a token for it.",
			h.restartURL())
		return
	}

	rawToken, err := h.mintFn(installID, accountLogin)
	if err != nil {
		log.Printf("install callback: mint: %v", err)
		h.renderError(c, http.StatusInternalServerError, "mint_failed",
			"Hub generated your token but failed to persist it.",
			"")
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

// errorCopy is the per-code presentation data the install_error.html template
// renders alongside the per-request longMessage.
type errorCopy struct {
	title      string
	steps      []string
	retryLabel string
}

// installErrorCopy maps a stable error code to the title + next-step bullets
// + retry-button label rendered on the install_error.html page. Adding a new
// error code means adding a new entry here — the Handle method calls
// renderError with that code and a per-request longMessage.
var installErrorCopy = map[string]errorCopy{
	"missing_installation_id": {
		title: "Install link is missing the installation id",
		steps: []string{
			"Re-open the install flow from your agent — run the login tool, or click the install link in your hub's docs.",
			"If you opened this URL by hand, the installation_id query parameter is required. The usual cause is a stale browser tab from a half-completed install.",
		},
		retryLabel: "Restart login",
	},
	"missing_oauth_code": {
		title: "GitHub didn't send an OAuth code",
		steps: []string{
			`Operator: enable "Request user authorization (OAuth) during installation" in this App's settings (Settings → Developer settings → GitHub Apps → Identifying and authorizing users).`,
			"After flipping the checkbox, reinstall the App on the target repo from github.com/settings/installations — GitHub will redirect back here with a fresh code.",
			"See docs/install/SELF-HOST.md for screenshots and the exact setting names.",
		},
		retryLabel: "Restart login",
	},
	"unsupported_setup_action": {
		title: "Unexpected setup_action",
		steps: []string{
			"GitHub only sends setup_action=install or setup_action=update to this URL — anything else means a hand-crafted request or a misconfigured App.",
			"Restart the install from your agent.",
		},
		retryLabel: "Restart login",
	},
	"no_credentials": {
		title: "Hub OAuth credentials aren't configured",
		steps: []string{
			"Operator: set CAW_APP_CLIENT_ID and CAW_APP_CLIENT_SECRET on the hub, or complete the manifest flow at /github/app/manifest.",
			"See docs/install/SELF-HOST.md for the full self-host setup walkthrough.",
			"Once credentials are present, restart the install from your agent.",
		},
		retryLabel: "Restart login",
	},
	"creds_lookup_failed": {
		title: "Hub couldn't load its OAuth credentials",
		steps: []string{
			"This is a hub-side internal error — usually a database read failure.",
			"Operator: check the hub logs for the install-callback line that preceded this page.",
			"Wait a minute and try again.",
		},
		retryLabel: "Restart login",
	},
	"oauth_exchange_failed": {
		title: "OAuth exchange with GitHub failed",
		steps: []string{
			"GitHub refused the OAuth code — most likely it expired (codes are good for ~10 minutes) or was reused.",
			"Restart the install from your agent so GitHub issues a fresh code.",
		},
		retryLabel: "Restart login",
	},
	"installations_lookup_failed": {
		title: "GitHub API call failed",
		steps: []string{
			"Hub couldn't reach GET /user/installations on GitHub — usually a transient GitHub outage.",
			"Wait 30 seconds and restart the install from your agent.",
		},
		retryLabel: "Restart login",
	},
	"not_an_admin": {
		title: "You don't have admin access to this installation",
		steps: []string{
			"This installation belongs to an org or account you can't admin — only an admin can mint a token for it.",
			"If you meant to install on a personal repo, restart and pick your personal account on the GitHub install page.",
			"If you're the right user, sign in to GitHub as the admin account first, then restart from your agent.",
		},
		retryLabel: "Restart login as the right account",
	},
	"mint_failed": {
		title: "Hub couldn't mint your token",
		steps: []string{
			"This is a hub-side internal error — usually a database write failure.",
			"Operator: check the hub logs for the install-callback line that preceded this page.",
			"Wait a minute and try again.",
		},
		retryLabel: "Restart login",
	},
}

// restartURL is where the "Restart login" button on every error page points.
// In Phase 3 of Auth v2 this lands on the real /auth/start-help handler that
// tells the user to run the `login` tool in their agent; in Phase 0 the URL
// 404s, which is acceptable because the on-page bullets already carry the
// actionable guidance. Centralising lets Phase 3 rename in one place.
func (h *InstallCallbackHandler) restartURL() string {
	return h.baseURL + "/auth/start-help"
}

// renderError writes install_error.html with HTTP status `status`. `code` is a
// stable identifier (logged + shown as a small badge on the page) used to look
// up the title + next-step bullets + retry-button label from installErrorCopy.
// `longMessage` is the per-request sentence describing what specifically went
// wrong. `retryURL` is the button target — empty means render no button.
func (h *InstallCallbackHandler) renderError(c *gin.Context, status int, code, longMessage, retryURL string) {
	cp, ok := installErrorCopy[code]
	if !ok {
		cp = errorCopy{
			title: "Install failed",
			steps: []string{
				"Restart the install from your agent.",
				"If it keeps failing, capture the URL and the time and contact the hub operator.",
			},
			retryLabel: "Restart login",
		}
	}
	h.renderInfoPage(c, status, code, cp.title, longMessage, cp.steps, retryURL, cp.retryLabel)
}

// renderSetupActionUpdate handles GitHub's setup_action=update redirect, which
// fires when an existing installation is reconfigured (e.g. repo added/removed
// from an installation's repository set). No new token is minted — the existing
// token still works. The page tells the user where to manage tokens (Phase 4
// of Auth v2 lands the /me/tokens route; until then the button 404s and we
// point self-hosters at `hub mint-token`).
func (h *InstallCallbackHandler) renderSetupActionUpdate(c *gin.Context) {
	h.renderInfoPage(c, http.StatusOK, "setup_action_update",
		"Installation updated",
		"Your GitHub App installation was reconfigured. No new token was minted — your existing token still works.",
		[]string{
			"To rotate or revoke tokens, head to /me/tokens (this route lands in Phase 4 of Auth v2 — the button below will 404 until then).",
			"Self-hosters can rotate now via: hub mint-token <installation_id> <org>.",
			"To get a fresh token from scratch, run the login tool in your agent.",
		},
		h.baseURL+"/me/tokens", "Manage tokens")
}

// renderInfoPage writes install_error.html with the given content. The CSP
// blocks scripts entirely (the error template has no inline JS, unlike the
// happy-path token reveal page), and Cache-Control: no-store keeps any
// transient query-string echo out of intermediate caches.
func (h *InstallCallbackHandler) renderInfoPage(c *gin.Context, status int, code, title, longMessage string, steps []string, retryURL, retryLabel string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Security-Policy",
		"default-src 'self'; style-src 'unsafe-inline'; img-src 'none'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(status)
	c.Header("Content-Type", "text/html; charset=utf-8")

	data := struct {
		HubURL      string
		Code        string
		Title       string
		LongMessage string
		Steps       []string
		RetryURL    string
		RetryLabel  string
	}{
		HubURL:      h.baseURL,
		Code:        code,
		Title:       title,
		LongMessage: longMessage,
		Steps:       steps,
		RetryURL:    retryURL,
		RetryLabel:  retryLabel,
	}
	if err := h.errorTmpl.Execute(c.Writer, data); err != nil {
		// Headers are already written; best we can do is log.
		log.Printf("install callback: render error page: %v", err)
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
