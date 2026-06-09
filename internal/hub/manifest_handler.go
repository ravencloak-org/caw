// Package hub — GitHub App Manifest flow HTTP handlers (Slice 5).
package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/githubapp"
	"github.com/ravencloak-org/caw/internal/store"
)

// ManifestHandler serves the self-host GitHub App Manifest flow.
//
//	GET  /github/app/manifest  → serves the manifest JSON (or redirects the browser to github.com/apps/new)
//	GET  /github/app/callback  → receives ?code=, exchanges it, stores credentials, mints a setup token
type ManifestHandler struct {
	baseURL      string // publicly reachable URL of this Hub (CAW_BASE_URL)
	githubBase   string // GitHub web base, defaults to https://github.com
	st           *store.Store
	mintFn       func(installationID, org string) (string, error) // may be nil
	manifestJSON []byte                                           // pre-encoded manifest
}

// ManifestConfig is the configuration for a ManifestHandler.
type ManifestConfig struct {
	BaseURL    string
	GithubBase string // defaults to "https://github.com"
	Store      *store.Store
	MintFn     func(installationID, org string) (string, error)
}

// NewManifestHandler constructs a ManifestHandler from cfg.
func NewManifestHandler(cfg ManifestConfig) (*ManifestHandler, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("NewManifestHandler: BaseURL is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("NewManifestHandler: Store is required")
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
		"redirect_url": callbackURL,
		"public":       false,
		"default_permissions": map[string]any{
			"pull_requests": "read",
			"checks":        "read",
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
		baseURL:      cfg.BaseURL,
		githubBase:   gh,
		st:           cfg.Store,
		mintFn:       cfg.MintFn,
		manifestJSON: b,
	}, nil
}

// HandleManifest serves GET /github/app/manifest.
// When the browser follows the link it is redirected to the GitHub App creation
// form with the manifest pre-filled via a POST form (see HTML body below).
func (m *ManifestHandler) HandleManifest(c *gin.Context) {
	newAppURL := m.githubBase + "/apps/new"
	// Build a self-submitting HTML form so the manifest is POST'd to GitHub.
	html := `<!DOCTYPE html><html><body><form id="f" method="post" action="` +
		htmlEscape(newAppURL) + `">` +
		`<input type="hidden" name="manifest" value="` +
		htmlAttrEscape(string(m.manifestJSON)) + `">` +
		`</form><script>document.getElementById("f").submit();</script></body></html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// HandleCallback handles GET /github/app/callback?code=<code>.
// It exchanges the code for GitHub App credentials, persists them, then mints
// a Hub token if a mintFn is configured.
func (m *ManifestHandler) HandleCallback(c *gin.Context) {
	code := c.Query("code")
	if code == "" {
		c.String(http.StatusBadRequest, "missing code")
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

	// Mint a setup token so the operator can immediately configure a Watcher.
	// Raw token is shown once in the response; hash is stored.
	if m.mintFn != nil {
		raw, err := m.mintFn("setup", "")
		if err != nil {
			log.Printf("manifest callback mint: %v", err) // non-fatal
		} else {
			// Show raw token once; never log it.
			c.String(http.StatusOK, "GitHub App registered.\n\nSetup token (shown once):\n%s\n", raw)
			return
		}
	}
	c.String(http.StatusOK, "GitHub App registered.")
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
