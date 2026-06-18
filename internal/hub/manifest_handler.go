// Package hub — GitHub App Manifest flow HTTP handlers (Slice 5).
package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/githubapp"
	"github.com/ravencloak-org/caw/internal/store"
)

// stateCookieName is the cookie that binds a manifest registration to its
// callback, defeating CSRF on the OAuth-style flow.
const stateCookieName = "caw_manifest_state"

// stateBytes is the number of cryptographically random bytes in a state nonce.
const stateBytes = 32

// ManifestHandler serves the self-host GitHub App Manifest flow.
//
// Both routes are an UNAUTHENTICATED-by-default surface that mints credentials
// and tokens, so they are gated behind an operator bootstrap secret:
//
//	GET  /github/app/manifest  → (gated) sets a CSRF state cookie and serves the
//	                             manifest form that POSTs to github.com/apps/new
//	GET  /github/app/callback  → (gated by state cookie) receives ?code=&state=,
//	                             exchanges it, stores credentials, confirms only.
//
// The bootstrap secret is valid only until the first SaveAppCredentials
// succeeds; thereafter the handler refuses to overwrite existing credentials
// unless allowRebootstrap is set. The raw setup token is NEVER echoed in the
// callback response — the operator retrieves it via the authenticated CLI path
// (`hub mint-token`).
type ManifestHandler struct {
	baseURL          string // publicly reachable URL of this Hub (CAW_BASE_URL)
	githubBase       string // GitHub web base, defaults to https://github.com
	st               *store.Store
	mintFn           func(installationID, org string) (string, error) // may be nil
	manifestJSON     []byte                                           // pre-encoded manifest (sans state)
	bootstrapToken   string                                           // operator bootstrap secret (required)
	allowRebootstrap bool                                             // permit overwriting existing credentials
	secureCookie     bool                                             // emit Secure cookie attribute (false in plain-HTTP tests)
}

// ManifestConfig is the configuration for a ManifestHandler.
type ManifestConfig struct {
	BaseURL    string
	GithubBase string // defaults to "https://github.com"
	Store      *store.Store
	MintFn     func(installationID, org string) (string, error)
	// BootstrapToken is the operator secret that gates both manifest routes.
	// Required: without it the handler cannot be constructed.
	BootstrapToken string
	// AllowRebootstrap permits overwriting credentials that already exist.
	// Off by default so a stolen bootstrap token cannot replace a live App.
	AllowRebootstrap bool
}

// NewManifestHandler constructs a ManifestHandler from cfg.
func NewManifestHandler(cfg ManifestConfig) (*ManifestHandler, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("NewManifestHandler: BaseURL is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("NewManifestHandler: Store is required")
	}
	if cfg.BootstrapToken == "" {
		return nil, fmt.Errorf("NewManifestHandler: BootstrapToken is required (gates the credential-minting routes)")
	}
	gh := cfg.GithubBase
	if gh == "" {
		gh = "https://github.com"
	}

	callbackURL := cfg.BaseURL + "/github/app/callback"

	manifest := map[string]any{
		"name": "Caw",
		"url":  cfg.BaseURL,
		"hook_attributes": map[string]any{
			"url":    cfg.BaseURL + "/webhooks/github",
			"active": true,
		},
		"redirect_url":             callbackURL,
		"setup_url":                cfg.BaseURL + "/github/app/install/callback", // ADR-0010: self-service Watcher token issuance
		"setup_on_update":          false,
		"request_oauth_on_install": true, // required so /install/callback receives ?code= for ownership check
		"public":                   false,
		"default_permissions": map[string]any{
			"pull_requests": "write", // read PR state + enable auto-merge (ADR-0002)
			"checks":        "read",
			"contents":      "write", // force-push rebased commits during orphan rebase
		},
		"default_events": []string{
			"check_suite",
			"pull_request",
			"pull_request_review",
			"pull_request_review_comment",
			"issue_comment",
			"installation",
			"installation_repositories",
		},
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("NewManifestHandler encode manifest: %w", err)
	}

	return &ManifestHandler{
		baseURL:          cfg.BaseURL,
		githubBase:       gh,
		st:               cfg.Store,
		mintFn:           cfg.MintFn,
		manifestJSON:     b,
		bootstrapToken:   cfg.BootstrapToken,
		allowRebootstrap: cfg.AllowRebootstrap,
		secureCookie:     true,
	}, nil
}

// authorizeBootstrap reports whether the request carries the operator bootstrap
// secret. The comparison is constant-time. It writes a 401 and returns false on
// failure.
func (m *ManifestHandler) authorizeBootstrap(c *gin.Context) bool {
	presented := bootstrapTokenFromRequest(c)
	if presented == "" ||
		subtle.ConstantTimeCompare([]byte(presented), []byte(m.bootstrapToken)) != 1 {
		c.String(http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// bootstrapTokenFromRequest extracts the bootstrap token from the Authorization
// bearer header, falling back to X-Caw-Token. It returns "" when neither is set.
func bootstrapTokenFromRequest(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && h[:len(prefix)] == prefix {
			return h[len(prefix):]
		}
	}
	return c.GetHeader("X-Caw-Token")
}

// credentialsExist reports whether App credentials are already persisted.
func (m *ManifestHandler) credentialsExist() (bool, error) {
	_, ok, err := m.st.LoadAppCredentials()
	return ok, err
}

// HandleManifest serves GET /github/app/manifest.
//
// It is gated by the operator bootstrap token. When credentials already exist
// and re-bootstrap is disabled it refuses with 403 (no overwrite). On success
// it mints a CSRF state nonce, sets it as a Secure, HttpOnly, SameSite=Lax
// cookie, embeds it in the manifest redirect_url, and serves a self-submitting
// HTML form that POSTs the manifest to GitHub.
func (m *ManifestHandler) HandleManifest(c *gin.Context) {
	if !m.authorizeBootstrap(c) {
		return
	}

	exists, err := m.credentialsExist()
	if err != nil {
		log.Printf("manifest: load credentials: %v", err)
		c.String(http.StatusInternalServerError, "store failed")
		return
	}
	if exists && !m.allowRebootstrap {
		c.String(http.StatusForbidden,
			"App credentials already exist; refusing to overwrite. Set ALLOW_REBOOTSTRAP=1 to re-register.")
		return
	}

	state, err := newState()
	if err != nil {
		log.Printf("manifest: generate state: %v", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}

	// Bind the registration to this browser via a state cookie. The same value
	// is embedded in the redirect_url so GitHub echoes it back on the callback.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(stateCookieName, state, 600 /*sec*/, "/github/app/", "", m.secureCookie, true /*HttpOnly*/)

	redirectURL := m.baseURL + "/github/app/callback?state=" + url.QueryEscape(state)
	manifestJSON, err := m.manifestWithRedirect(redirectURL)
	if err != nil {
		log.Printf("manifest: encode: %v", err)
		c.String(http.StatusInternalServerError, "internal error")
		return
	}

	newAppURL := m.githubBase + "/apps/new"
	// Build a self-submitting HTML form so the manifest is POST'd to GitHub.
	html := `<!DOCTYPE html><html><body><form id="f" method="post" action="` +
		htmlEscape(newAppURL) + `">` +
		`<input type="hidden" name="manifest" value="` +
		htmlAttrEscape(string(manifestJSON)) + `">` +
		`</form><script>document.getElementById("f").submit();</script></body></html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// manifestWithRedirect returns the pre-encoded manifest with its redirect_url
// replaced by redirectURL (which carries the CSRF state param).
func (m *ManifestHandler) manifestWithRedirect(redirectURL string) ([]byte, error) {
	var manifest map[string]any
	if err := json.Unmarshal(m.manifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	manifest["redirect_url"] = redirectURL
	b, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return b, nil
}

// HandleCallback handles GET /github/app/callback?code=<code>&state=<state>.
//
// Authorization is carried by the CSRF state cookie: it is set only by an
// operator-authenticated HandleManifest, so a valid cookie proves the flow was
// initiated by an authorized operator. The state query param and cookie must
// match (constant-time). It exchanges the code for App credentials, refuses to
// overwrite existing credentials unless re-bootstrap is allowed, persists them,
// and confirms success WITHOUT echoing any raw token.
func (m *ManifestHandler) HandleCallback(c *gin.Context) {
	// CSRF / authorization: the state cookie is only set by an authorized
	// HandleManifest, so validating it both defeats CSRF and gates the route.
	cookieState, err := c.Cookie(stateCookieName)
	if err != nil || cookieState == "" {
		c.String(http.StatusBadRequest, "missing state")
		return
	}
	// Clear the cookie after use regardless of outcome (single-use).
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(stateCookieName, "", -1, "/github/app/", "", m.secureCookie, true)

	queryState := c.Query("state")
	if queryState == "" ||
		subtle.ConstantTimeCompare([]byte(queryState), []byte(cookieState)) != 1 {
		c.String(http.StatusBadRequest, "state mismatch")
		return
	}

	code := c.Query("code")
	if code == "" {
		c.String(http.StatusBadRequest, "missing code")
		return
	}

	// Re-check overwrite protection at exchange time (TOCTOU-safe enough for a
	// single-operator bootstrap surface).
	exists, err := m.credentialsExist()
	if err != nil {
		log.Printf("manifest callback: load credentials: %v", err)
		c.String(http.StatusInternalServerError, "store failed")
		return
	}
	if exists && !m.allowRebootstrap {
		c.String(http.StatusForbidden,
			"App credentials already exist; refusing to overwrite.")
		return
	}

	creds, err := githubapp.ConvertManifest(c.Request.Context(), m.githubBase, code)
	if err != nil {
		log.Printf("manifest callback ConvertManifest: %v", err)
		c.String(http.StatusBadGateway, "exchange failed")
		return
	}

	sc := store.AppCredentials{
		AppID:         fmt.Sprintf("%d", creds.AppID),
		ClientID:      creds.ClientID,
		ClientSecret:  creds.ClientSecret,
		WebhookSecret: creds.WebhookSecret,
		PEM:           creds.PEM,
	}
	if err := m.st.SaveAppCredentials(sc); err != nil {
		log.Printf("manifest callback SaveAppCredentials: %v", err)
		c.String(http.StatusInternalServerError, "store failed")
		return
	}

	// Mint a setup token so the operator can configure a Watcher. The raw token
	// is NEVER returned here (it would leak through the unauthenticated GitHub
	// redirect chain). The operator retrieves it via `hub mint-token`.
	if m.mintFn != nil {
		if _, err := m.mintFn("setup", ""); err != nil {
			log.Printf("manifest callback mint: %v", err) // non-fatal
		}
	}

	c.String(http.StatusOK,
		"GitHub App registered. Retrieve a setup token with: hub mint-token <installation_id> [org]")
}

// newState returns a base64-encoded 32-byte random CSRF nonce.
func newState() (string, error) {
	b := make([]byte, stateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// htmlEscape escapes characters that are special in HTML attribute values.
func htmlEscape(s string) string {
	return url.PathEscape(s)
}

// htmlAttrEscape escapes < > " & for use inside an HTML attribute.
func htmlAttrEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			out = append(out, '&', 'l', 't', ';')
		case '>':
			out = append(out, '&', 'g', 't', ';')
		case '"':
			out = append(out, '&', 'q', 'u', 'o', 't', ';')
		case '&':
			out = append(out, '&', 'a', 'm', 'p', ';')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
