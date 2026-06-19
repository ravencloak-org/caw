// Package hub — Auth v2 MCP-initiated login HTTP handlers (Phase 3).
//
// This file owns the seven /auth/* routes from the issue #59 wire protocol:
//
//	POST /auth/start          — MCP kicks off a login; returns session_id +
//	                            verification_url (loopback) or device_code +
//	                            user_code + verification_uri[_complete] (device).
//	GET  /auth/u/:session_id  — browser entrypoint; sets a session cookie and
//	                            302s to GitHub OAuth.
//	GET  /auth/cb/github      — GitHub OAuth callback; branches to picker or
//	                            install-redirect.
//	GET  /auth/picker/:id     — renders the multi-installation picker (HTML).
//	POST /auth/picker/:id     — picker submit; mints tokens and delivers the
//	                            bundle to the MCP (loopback POST or device-flow
//	                            buffered pickup).
//	GET  /auth/device         — device-flow form where the user pastes user_code.
//	POST /auth/poll           — device-flow polling; returns the TokenBundle.
//	GET  /auth/done/:id       — browser polling endpoint the success page hits.
//	GET  /auth/start-help     — static "run the login tool" help page.
//
// The full wire protocol explainer lives in the Phase 3 issue + the master
// plan; the inline doc on each handler stays terse and points at the section
// numbers there rather than restating the protocol.
package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

//go:embed auth_picker.html
var authPickerHTML string

//go:embed auth_success.html
var authSuccessHTML string

//go:embed auth_device.html
var authDeviceHTML string

//go:embed auth_start_help.html
var authStartHelpHTML string

// Cookie + session timing constants.
const (
	authSessionCookieName = "caw_auth_session"
	// authSessionTTL bounds how long a /auth/start session stays valid before
	// it expires. Matches the 10-minute clamp from the plan's wire protocol.
	authSessionTTL = 10 * time.Minute
	// authPollInterval is the recommended device-flow poll cadence — the
	// /auth/start response advertises it so the MCP knows to wait this long.
	authPollInterval = 5
	// authPollSlowDownDelta is the minimum gap between two /auth/poll calls
	// before the hub answers slow_down. Stricter than the interval so a
	// client that exactly hits the interval is not penalized.
	authPollSlowDownDelta = 4 * time.Second
	// authLoopbackTimeout caps the hub's POST to the MCP's loopback listener.
	// If the listener is dead the user still sees success but the MCP gives
	// up after its own 5-min wait — no point holding the picker request open.
	authLoopbackTimeout = 5 * time.Second
)

// userCodeAlphabet is the subset of A-Z + digits we use for user_code, with
// 0/O/1/I/L removed because they look alike in monospace fonts and the user
// types these by hand.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// AuthSessionHandlerConfig is the configuration for an AuthSessionHandler.
type AuthSessionHandlerConfig struct {
	// BaseURL is this Hub's public URL, used to construct verification_url +
	// the GitHub OAuth redirect_uri.
	BaseURL string
	// GithubBase is github.com (configurable for tests). Defaults to
	// https://github.com.
	GithubBase string
	// APIBase is api.github.com. Defaults to https://api.github.com.
	APIBase string
	// Store persists auth_sessions + (via MintFn) tokens.
	Store *store.Store
	// MintFn mints user-bound tokens — same signature as install_callback's.
	// Phase 3 finally calls this with non-zero userID + a user-supplied
	// device_label, populating the (github_user_id, installation_id,
	// device_label) tuple Phase 5 will start enforcing.
	MintFn MintFunc
	// CredsFn resolves the App's OAuth client_id/client_secret at request
	// time — same shape install_callback uses; main.go stacks env over store.
	CredsFn func() (clientID, clientSecret string, ok bool, err error)
	// AppSlugFn returns the GitHub App URL slug (e.g. "caw-ravencloak"),
	// used to build https://github.com/apps/<slug>/installations/new when the
	// user finishes OAuth with zero installations. Env CAW_APP_SLUG wins;
	// store.AnyAppSlug is the fallback. Returning "" disables the install
	// redirect branch — the handler renders an actionable error instead.
	AppSlugFn func() string
	// HTTPClient overrides the HTTP client used for GitHub + loopback calls.
	// Defaults to http.DefaultClient; tests inject a stub.
	HTTPClient HTTPDoer
	// SecureCookie sets the Secure flag on caw_auth_session. Off in plain-
	// HTTP tests; on in production (BaseURL with https:// scheme).
	SecureCookie bool
	// Now is the clock seam — tests pin time, prod uses time.Now.
	Now func() time.Time
}

// AuthSessionHandler serves the /auth/* surface. All long-lived state lives
// on auth_sessions; the only in-memory state is the device-flow rate-limit
// map below.
type AuthSessionHandler struct {
	cfg          AuthSessionHandlerConfig
	pickerTmpl   *template.Template
	successTmpl  *template.Template
	deviceTmpl   *template.Template
	helpTmpl     *template.Template
	httpClient   HTTPDoer
	now          func() time.Time
	pollLastSeen sync.Map // map[deviceCode]time.Time
}

// NewAuthSessionHandler validates cfg and returns a wired handler. It is
// constructed once at hub boot; main.go owns the lifetime.
func NewAuthSessionHandler(cfg AuthSessionHandlerConfig) (*AuthSessionHandler, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("NewAuthSessionHandler: BaseURL is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("NewAuthSessionHandler: Store is required")
	}
	if cfg.MintFn == nil {
		return nil, fmt.Errorf("NewAuthSessionHandler: MintFn is required")
	}
	if cfg.CredsFn == nil {
		return nil, fmt.Errorf("NewAuthSessionHandler: CredsFn is required")
	}
	if cfg.AppSlugFn == nil {
		cfg.AppSlugFn = func() string { return "" }
	}
	if cfg.GithubBase == "" {
		cfg.GithubBase = "https://github.com"
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	pickerTmpl, err := template.New("auth_picker").Parse(authPickerHTML)
	if err != nil {
		return nil, fmt.Errorf("parse auth_picker.html: %w", err)
	}
	successTmpl, err := template.New("auth_success").Parse(authSuccessHTML)
	if err != nil {
		return nil, fmt.Errorf("parse auth_success.html: %w", err)
	}
	deviceTmpl, err := template.New("auth_device").Parse(authDeviceHTML)
	if err != nil {
		return nil, fmt.Errorf("parse auth_device.html: %w", err)
	}
	helpTmpl, err := template.New("auth_start_help").Parse(authStartHelpHTML)
	if err != nil {
		return nil, fmt.Errorf("parse auth_start_help.html: %w", err)
	}
	return &AuthSessionHandler{
		cfg:         cfg,
		pickerTmpl:  pickerTmpl,
		successTmpl: successTmpl,
		deviceTmpl:  deviceTmpl,
		helpTmpl:    helpTmpl,
		httpClient:  cfg.HTTPClient,
		now:         cfg.Now,
	}, nil
}

// startRequest is the POST /auth/start body. mode is required; the other
// fields' presence depends on mode (loopback wants loopback_redirect, device
// wants nothing extra).
type startRequest struct {
	Mode                string `json:"mode"`
	LoopbackRedirect    string `json:"loopback_redirect"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	ClientLabel         string `json:"client_label"`
}

// startLoopbackResponse is the /auth/start response shape for mode=loopback.
type startLoopbackResponse struct {
	SessionID       string `json:"session_id"`
	VerificationURL string `json:"verification_url"`
	ExpiresAt       int64  `json:"expires_at"`
}

// startDeviceResponse is the /auth/start response shape for mode=device.
type startDeviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresAt               int64  `json:"expires_at"`
	Interval                int    `json:"interval"`
}

// HandleStart serves POST /auth/start (loopback + device flows).
func (h *AuthSessionHandler) HandleStart(c *gin.Context) {
	var req startRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if req.ClientLabel == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "client_label required"})
		return
	}
	if len(req.ClientLabel) > 64 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "client_label exceeds 64 chars"})
		return
	}
	if req.CodeChallenge == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "code_challenge required"})
		return
	}
	if !strings.EqualFold(req.CodeChallengeMethod, auth.PKCEMethod) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "code_challenge_method must be S256"})
		return
	}
	if req.Mode != "loopback" && req.Mode != "device" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "mode must be loopback or device"})
		return
	}

	sessionID, err := auth.GenerateID()
	if err != nil {
		log.Printf("auth start: GenerateID: %v", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "session_id_alloc_failed"})
		return
	}

	now := h.now()
	row := store.AuthSession{
		ID:                  sessionID,
		HandshakeMode:       req.Mode,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: auth.PKCEMethod,
		ClientLabel:         req.ClientLabel,
		State:               "pending",
		CreatedAt:           now.Unix(),
		ExpiresAt:           now.Add(authSessionTTL).Unix(),
	}

	switch req.Mode {
	case "loopback":
		if err := validateLoopbackRedirect(req.LoopbackRedirect); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		row.LoopbackRedirect = req.LoopbackRedirect
		if err := h.cfg.Store.InsertAuthSession(row); err != nil {
			log.Printf("auth start (loopback): InsertAuthSession: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "session_insert_failed"})
			return
		}
		c.JSON(http.StatusOK, startLoopbackResponse{
			SessionID:       sessionID,
			VerificationURL: h.cfg.BaseURL + "/auth/u/" + sessionID,
			ExpiresAt:       row.ExpiresAt,
		})

	case "device":
		deviceCode, err := generateDeviceCode()
		if err != nil {
			log.Printf("auth start (device): generateDeviceCode: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "device_code_alloc_failed"})
			return
		}
		userCode, err := generateUserCode()
		if err != nil {
			log.Printf("auth start (device): generateUserCode: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "user_code_alloc_failed"})
			return
		}
		row.DeviceCode = deviceCode
		row.UserCode = userCode
		if err := h.cfg.Store.InsertAuthSession(row); err != nil {
			log.Printf("auth start (device): InsertAuthSession: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "session_insert_failed"})
			return
		}
		verificationURI := h.cfg.BaseURL + "/auth/device"
		c.JSON(http.StatusOK, startDeviceResponse{
			DeviceCode:              deviceCode,
			UserCode:                userCode,
			VerificationURI:         verificationURI,
			VerificationURIComplete: verificationURI + "?code=" + url.QueryEscape(userCode),
			ExpiresAt:               row.ExpiresAt,
			Interval:                authPollInterval,
		})
	}
}

// HandleBrowserStart serves GET /auth/u/:session_id — the browser entrypoint
// for the loopback flow. Sets the caw_auth_session cookie bound to the
// session id, then 302s to GitHub OAuth authorize.
func (h *AuthSessionHandler) HandleBrowserStart(c *gin.Context) {
	sessionID := c.Param("session_id")
	a, ok, err := h.cfg.Store.GetAuthSession(sessionID)
	if err != nil {
		log.Printf("auth browser start: GetAuthSession: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Session lookup failed.")
		return
	}
	if !ok {
		h.renderTextError(c, http.StatusNotFound, "Login session not found. Restart the login tool from your agent.")
		return
	}
	if h.now().Unix() >= a.ExpiresAt {
		h.renderTextError(c, http.StatusGone, "Login session expired. Restart the login tool from your agent.")
		return
	}
	if a.HandshakeMode != "loopback" {
		h.renderTextError(c, http.StatusBadRequest, "This URL is for the loopback flow only; device-flow users should visit /auth/device.")
		return
	}

	clientID, _, ok, err := h.cfg.CredsFn()
	if err != nil {
		log.Printf("auth browser start: CredsFn: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Hub couldn't load its App credentials.")
		return
	}
	if !ok || clientID == "" {
		h.renderTextError(c, http.StatusFailedDependency, "App OAuth credentials are not configured on this hub.")
		return
	}

	// Cookie path stays scoped to /auth so the rest of the hub never sees it.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(authSessionCookieName, sessionID,
		int(authSessionTTL.Seconds()), "/auth", "", h.cfg.SecureCookie, true)

	redirect := h.githubOAuthURL(clientID, sessionID)
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, redirect)
}

// HandleGithubCallback serves GET /auth/cb/github.
//
// Exchanges the OAuth code for a user-to-server token, fetches /user and
// /user/installations, then branches:
//
//   - zero installs  → 302 to https://github.com/apps/<slug>/installations/new
//   - one or more    → store the installation list as JSON on
//     pending_bundle_json (so the picker handler reads it
//     back without re-fetching) and 302 to the picker.
func (h *AuthSessionHandler) HandleGithubCallback(c *gin.Context) {
	queryState := c.Query("state")
	if queryState == "" {
		h.renderTextError(c, http.StatusBadRequest, "Missing state from GitHub redirect.")
		return
	}
	cookieState, err := c.Cookie(authSessionCookieName)
	if err != nil || cookieState == "" {
		h.renderTextError(c, http.StatusBadRequest, "Missing session cookie. Restart the login flow.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(queryState), []byte(cookieState)) != 1 {
		h.renderTextError(c, http.StatusBadRequest, "Login state mismatch — possible CSRF. Restart the login flow.")
		return
	}

	a, ok, err := h.cfg.Store.GetAuthSession(queryState)
	if err != nil {
		log.Printf("auth cb: GetAuthSession: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Session lookup failed.")
		return
	}
	if !ok {
		h.renderTextError(c, http.StatusNotFound, "Login session not found. Restart the login tool from your agent.")
		return
	}
	if h.now().Unix() >= a.ExpiresAt {
		h.renderTextError(c, http.StatusGone, "Login session expired. Restart the login tool from your agent.")
		return
	}

	code := c.Query("code")
	if code == "" {
		h.renderTextError(c, http.StatusBadRequest, "Missing code from GitHub redirect.")
		return
	}

	clientID, clientSecret, credsOK, err := h.cfg.CredsFn()
	if err != nil {
		log.Printf("auth cb: CredsFn: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Hub couldn't load its App credentials.")
		return
	}
	if !credsOK {
		h.renderTextError(c, http.StatusFailedDependency, "App OAuth credentials are not configured.")
		return
	}

	ctx := c.Request.Context()
	userToken, err := exchangeOAuthCodeShared(ctx, h.httpClient, h.cfg.GithubBase, clientID, clientSecret, code)
	if err != nil {
		log.Printf("auth cb: exchangeOAuthCode: %v", err)
		c.Header("Retry-After", "10")
		h.renderTextError(c, http.StatusBadGateway, "OAuth exchange with GitHub failed. Retry from your agent.")
		return
	}
	user, err := fetchUser(ctx, h.httpClient, h.cfg.APIBase, userToken)
	if err != nil {
		log.Printf("auth cb: fetchUser: %v", err)
		c.Header("Retry-After", "10")
		h.renderTextError(c, http.StatusBadGateway, "Couldn't fetch your GitHub identity. Retry from your agent.")
		return
	}
	installs, err := listUserInstallations(ctx, h.httpClient, h.cfg.APIBase, userToken)
	if err != nil {
		log.Printf("auth cb: listUserInstallations: %v", err)
		c.Header("Retry-After", "10")
		h.renderTextError(c, http.StatusBadGateway, "Couldn't list your GitHub installations. Retry from your agent.")
		return
	}

	// Persist user id + login on the session row regardless of the install
	// branch — the install-resume flow (install_callback.go) needs them when
	// the user comes back from installing the App.
	if err := h.cfg.Store.SetSessionUser(a.ID, user.ID, user.Login); err != nil {
		log.Printf("auth cb: SetSessionUser: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Couldn't persist your GitHub identity to the session.")
		return
	}

	if len(installs) == 0 {
		slug := h.cfg.AppSlugFn()
		if slug == "" {
			h.renderTextError(c, http.StatusFailedDependency,
				"You have no installations of this App, and the hub has no App slug configured to send you to install one. Operator: set CAW_APP_SLUG.")
			return
		}
		if err := h.cfg.Store.UpdateAuthSessionState(a.ID, "awaiting_install"); err != nil {
			log.Printf("auth cb: UpdateAuthSessionState awaiting_install: %v", err)
		}
		installURL := fmt.Sprintf("%s/apps/%s/installations/new?state=%s",
			h.cfg.GithubBase, slug, url.QueryEscape(a.ID))
		c.Header("Cache-Control", "no-store")
		c.Redirect(http.StatusFound, installURL)
		return
	}

	// Stash the install list on the session so the picker handler — which
	// renders on a second GET — doesn't have to refetch with a token it
	// would also have to cache. Discarded once the picker submit lands.
	enc, err := json.Marshal(installs)
	if err != nil {
		log.Printf("auth cb: marshal installs: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Couldn't persist install list to session.")
		return
	}
	if err := h.cfg.Store.SetSessionPendingBundle(a.ID, string(enc)); err != nil {
		// SetSessionPendingBundle flips state to "delivered" — back it out.
		// In practice the picker handler will overwrite immediately.
		log.Printf("auth cb: SetSessionPendingBundle (installs): %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Couldn't persist install list to session.")
		return
	}
	// Re-state to awaiting_picker so HandlePicker can sanity-check.
	if err := h.cfg.Store.UpdateAuthSessionState(a.ID, "awaiting_picker"); err != nil {
		log.Printf("auth cb: UpdateAuthSessionState awaiting_picker: %v", err)
	}

	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, "/auth/picker/"+a.ID)
}

// HandlePickerGet renders the picker page (GET /auth/picker/:session_id).
// The page is one form with a checkbox per installation + a device_label
// input pre-filled with the client_label the MCP supplied at /auth/start.
func (h *AuthSessionHandler) HandlePickerGet(c *gin.Context) {
	sessionID := c.Param("session_id")
	a, installs, err := h.loadPickerSession(c, sessionID)
	if err != nil || installs == nil {
		return // loadPickerSession already wrote a response
	}
	h.renderPicker(c, a, installs, "")
}

// HandlePickerPost handles the picker form submit (POST /auth/picker/:id).
//
// On cancel=1: mark session canceled, fire loopback with {"error":"user_canceled"},
// render a cancellation page.
// On install_ids[] non-empty: mint one token per (user, install, label),
// build TokenBundle, persist on session, fire loopback (if mode=loopback),
// render auth_success.html.
func (h *AuthSessionHandler) HandlePickerPost(c *gin.Context) {
	sessionID := c.Param("session_id")
	a, installs, err := h.loadPickerSession(c, sessionID)
	if err != nil || installs == nil {
		return
	}

	if c.PostForm("cancel") == "1" {
		if err := h.cfg.Store.UpdateAuthSessionState(a.ID, "canceled"); err != nil {
			log.Printf("auth picker: mark canceled: %v", err)
		}
		if a.HandshakeMode == "loopback" && a.LoopbackRedirect != "" {
			h.fireLoopback(c.Request.Context(), a.LoopbackRedirect, map[string]any{
				"session_id":     a.ID,
				"code_challenge": a.CodeChallenge,
				"error":          "user_canceled",
			})
		}
		c.Header("Cache-Control", "no-store")
		h.renderTextError(c, http.StatusOK, "Login canceled. You can close this tab.")
		return
	}

	selectedIDs := normalizeInstallIDs(c.PostFormArray("installation_ids[]"))
	if len(selectedIDs) == 0 {
		// Empty selection counts as a user error, not cancel — re-render with msg.
		h.renderPicker(c, a, installs, "Pick at least one installation.")
		return
	}
	// Cross-check against installs the user actually has — protects against
	// a tampered form that posts an installation_id the user can't access.
	allowed := make(map[string]ghInstallation, len(installs))
	for _, in := range installs {
		allowed[strconv.FormatInt(in.ID, 10)] = in
	}
	for _, id := range selectedIDs {
		if _, ok := allowed[id]; !ok {
			h.renderPicker(c, a, installs, fmt.Sprintf("Installation %s is not in your list. Pick from the checkboxes.", id))
			return
		}
	}

	deviceLabel := strings.TrimSpace(c.PostForm("device_label"))
	if deviceLabel == "" {
		deviceLabel = a.ClientLabel
	}
	if len(deviceLabel) > 64 {
		deviceLabel = deviceLabel[:64]
	}

	userID := int64(0)
	if a.GitHubUserID != nil {
		userID = *a.GitHubUserID
	}
	if userID == 0 {
		h.renderTextError(c, http.StatusBadRequest, "Session has no GitHub user — the OAuth callback did not complete.")
		return
	}

	type tokenEntry struct {
		InstallationID string `json:"installation_id"`
		OwnerLogin     string `json:"owner_login"`
		Token          string `json:"token"`
		TokenID        string `json:"token_id"`
		ExpiresAt      int64  `json:"expires_at,omitempty"`
	}
	tokens := make([]tokenEntry, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		in := allowed[id]
		raw, tokenID, err := h.cfg.MintFn(id, in.Account.Login, deviceLabel, userID, a.GitHubUserLogin)
		if err != nil {
			log.Printf("auth picker: mint for install=%s user=%d: %v", id, userID, err)
			h.renderTextError(c, http.StatusInternalServerError, "Failed to mint a token. Restart the login tool from your agent.")
			return
		}
		tokens = append(tokens, tokenEntry{
			InstallationID: id,
			OwnerLogin:     in.Account.Login,
			Token:          raw,
			TokenID:        tokenID,
		})
	}

	bundle := map[string]any{
		"session_id":        a.ID,
		"code_challenge":    a.CodeChallenge,
		"github_user_id":    userID,
		"github_user_login": a.GitHubUserLogin,
		"tokens":            tokens,
	}
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		log.Printf("auth picker: marshal bundle: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Failed to encode token bundle.")
		return
	}
	if err := h.cfg.Store.SetSessionPendingBundle(a.ID, string(bundleJSON)); err != nil {
		log.Printf("auth picker: SetSessionPendingBundle: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Failed to persist token bundle.")
		return
	}

	if a.HandshakeMode == "loopback" && a.LoopbackRedirect != "" {
		// Fire-and-forget the loopback POST. The browser learns about success
		// via /auth/done/:id; the MCP learns directly via the loopback POST.
		// We log failures but never bubble them to the user — the device-flow
		// fallback path also stores the bundle for retrieval via /auth/poll.
		h.fireLoopback(c.Request.Context(), a.LoopbackRedirect, bundle)
	}

	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = h.successTmpl.Execute(c.Writer, map[string]any{
		"SessionID":     a.ID,
		"ClientLabel":   a.ClientLabel,
		"HandshakeMode": a.HandshakeMode,
		"TokenCount":    len(tokens),
		"HubURL":        h.cfg.BaseURL,
	})
}

// HandleDevice serves GET /auth/device — the device-flow form. If ?code=
// is present the form is pre-filled; otherwise the user pastes their
// user_code by hand. On submit (also a GET so the form action stays simple)
// the handler validates user_code, sets the session cookie bound to the
// device_code, and 302s through the same GitHub OAuth path as loopback.
func (h *AuthSessionHandler) HandleDevice(c *gin.Context) {
	userCode := strings.TrimSpace(c.Query("code"))
	if userCode == "" {
		c.Header("Cache-Control", "no-store")
		c.Header("Content-Type", "text/html; charset=utf-8")
		_ = h.deviceTmpl.Execute(c.Writer, map[string]any{
			"PrefilledCode": "",
			"ErrorMessage":  "",
		})
		return
	}

	// Normalize to upper-case (the alphabet is upper-case only).
	userCode = strings.ToUpper(userCode)
	a, ok, err := h.cfg.Store.GetSessionByUserCode(userCode)
	if err != nil {
		log.Printf("auth device: GetSessionByUserCode: %v", err)
		h.renderDeviceWithError(c, userCode, "Lookup failed; try again.")
		return
	}
	if !ok {
		h.renderDeviceWithError(c, userCode, "Unknown code. Re-check the value your agent printed.")
		return
	}
	if h.now().Unix() >= a.ExpiresAt {
		h.renderDeviceWithError(c, userCode, "Code expired. Restart the login tool from your agent.")
		return
	}

	clientID, _, ok, err := h.cfg.CredsFn()
	if err != nil {
		log.Printf("auth device: CredsFn: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Hub couldn't load its App credentials.")
		return
	}
	if !ok || clientID == "" {
		h.renderTextError(c, http.StatusFailedDependency, "App OAuth credentials are not configured on this hub.")
		return
	}

	// Cookie carries the session_id — same shape as loopback so /auth/cb/github
	// stays single-codepath. Device-flow callbacks land on the same handler.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(authSessionCookieName, a.ID,
		int(authSessionTTL.Seconds()), "/auth", "", h.cfg.SecureCookie, true)

	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, h.githubOAuthURL(clientID, a.ID))
}

// pollRequest is the POST /auth/poll body.
type pollRequest struct {
	DeviceCode   string `json:"device_code"`
	CodeVerifier string `json:"code_verifier"`
}

// HandlePoll serves POST /auth/poll. Returns the TokenBundle on success;
// JSON {"error":"..."} otherwise per the OAuth device-flow conventions
// (authorization_pending / slow_down / expired_token / access_denied).
func (h *AuthSessionHandler) HandlePoll(c *gin.Context) {
	var req pollRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	if req.DeviceCode == "" || req.CodeVerifier == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}

	// Slow_down: a client that polls faster than authPollSlowDownDelta gets
	// a 400 with code=slow_down. Tracked in-memory keyed on device_code.
	// Uses wall-clock time.Now (NOT h.now), because slow_down is a real-time
	// rate-limit and h.now is the session-expiry seam tests freeze.
	wallNow := time.Now()
	if v, ok := h.pollLastSeen.Load(req.DeviceCode); ok {
		if last, ok := v.(time.Time); ok {
			if wallNow.Sub(last) < authPollSlowDownDelta {
				h.pollLastSeen.Store(req.DeviceCode, wallNow)
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "slow_down"})
				return
			}
		}
	}
	h.pollLastSeen.Store(req.DeviceCode, wallNow)
	now := h.now()

	a, ok, err := h.cfg.Store.GetSessionByDeviceCode(req.DeviceCode)
	if err != nil {
		log.Printf("auth poll: GetSessionByDeviceCode: %v", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server_error"})
		return
	}
	if !ok {
		// Unknown device_code: indistinguishable from "expired and purged".
		c.AbortWithStatusJSON(http.StatusGone, gin.H{"error": "expired_token"})
		return
	}
	if now.Unix() >= a.ExpiresAt {
		c.AbortWithStatusJSON(http.StatusGone, gin.H{"error": "expired_token"})
		return
	}
	if a.State == "canceled" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access_denied"})
		return
	}

	if err := auth.VerifyPKCE(req.CodeVerifier, a.CodeChallenge, a.CodeChallengeMethod); err != nil {
		// A bad code_verifier here is the protocol-defined "access_denied" —
		// the device polling client is the wrong party (or tampered).
		log.Printf("auth poll: VerifyPKCE: %v", err)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access_denied"})
		return
	}

	if a.State != "delivered" || a.PendingBundleJSON == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "authorization_pending"})
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", []byte(a.PendingBundleJSON))
}

// HandleDone serves GET /auth/done/:session_id — the browser-side polling
// endpoint the success page hits to confirm the loopback fire-and-forget
// landed (or the device-flow poll consumed). 200 once state=delivered,
// 202 while pending, 404 if unknown, 410 if expired/canceled.
func (h *AuthSessionHandler) HandleDone(c *gin.Context) {
	sessionID := c.Param("session_id")
	a, ok, err := h.cfg.Store.GetAuthSession(sessionID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "server_error"})
		return
	}
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	switch a.State {
	case "delivered":
		c.JSON(http.StatusOK, gin.H{"state": "delivered"})
	case "canceled", "expired":
		c.JSON(http.StatusGone, gin.H{"state": a.State})
	default:
		c.JSON(http.StatusAccepted, gin.H{"state": a.State})
	}
}

// HandleStartHelp serves GET /auth/start-help — a static page restartable
// from any of install_callback.go's error renderings. Tells the user to run
// the `login` tool inside their MCP-driven agent.
func (h *AuthSessionHandler) HandleStartHelp(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = h.helpTmpl.Execute(c.Writer, map[string]any{
		"HubURL": h.cfg.BaseURL,
	})
}

// --- helpers ---

// loadPickerSession centralizes the picker handlers' precondition checks:
// session exists, not expired, in awaiting_picker state, and pending_bundle_json
// holds the install list cached by HandleGithubCallback. Returns the row + list
// on success; writes a response and returns (zero,nil,nil) otherwise.
func (h *AuthSessionHandler) loadPickerSession(c *gin.Context, sessionID string) (store.AuthSession, []ghInstallation, error) {
	a, ok, err := h.cfg.Store.GetAuthSession(sessionID)
	if err != nil {
		h.renderTextError(c, http.StatusInternalServerError, "Session lookup failed.")
		return store.AuthSession{}, nil, err
	}
	if !ok {
		h.renderTextError(c, http.StatusNotFound, "Login session not found. Restart from your agent.")
		return store.AuthSession{}, nil, nil
	}
	if h.now().Unix() >= a.ExpiresAt {
		h.renderTextError(c, http.StatusGone, "Login session expired. Restart from your agent.")
		return store.AuthSession{}, nil, nil
	}
	if a.State != "awaiting_picker" {
		h.renderTextError(c, http.StatusConflict,
			"Login session is not in the picker state — it may already be delivered or canceled.")
		return store.AuthSession{}, nil, nil
	}
	if a.PendingBundleJSON == "" {
		h.renderTextError(c, http.StatusConflict, "Session is missing its install list — restart from your agent.")
		return store.AuthSession{}, nil, nil
	}
	var installs []ghInstallation
	if err := json.Unmarshal([]byte(a.PendingBundleJSON), &installs); err != nil {
		log.Printf("auth picker: unmarshal install list: %v", err)
		h.renderTextError(c, http.StatusInternalServerError, "Cached install list is corrupted.")
		return store.AuthSession{}, nil, err
	}
	return a, installs, nil
}

// renderPicker renders auth_picker.html with the given installs + an optional
// banner message (used for inline form-validation errors).
func (h *AuthSessionHandler) renderPicker(c *gin.Context, a store.AuthSession, installs []ghInstallation, message string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	type pickerInstall struct {
		ID    string
		Login string
		Type  string
	}
	rendered := make([]pickerInstall, 0, len(installs))
	for _, in := range installs {
		rendered = append(rendered, pickerInstall{
			ID:    strconv.FormatInt(in.ID, 10),
			Login: in.Account.Login,
			Type:  in.Account.Type,
		})
	}
	_ = h.pickerTmpl.Execute(c.Writer, map[string]any{
		"SessionID":    a.ID,
		"UserLogin":    a.GitHubUserLogin,
		"ClientLabel":  a.ClientLabel,
		"Installs":     rendered,
		"Message":      message,
		"PickerAction": "/auth/picker/" + a.ID,
	})
}

// renderDeviceWithError re-renders the device form with a banner error.
func (h *AuthSessionHandler) renderDeviceWithError(c *gin.Context, code, msg string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = h.deviceTmpl.Execute(c.Writer, map[string]any{
		"PrefilledCode": code,
		"ErrorMessage":  msg,
	})
}

// renderTextError writes a minimal HTML error page (no template parse on hot
// path; these errors are rare). Content-Type is text/html so browsers render
// it the same as the rest of the /auth/* surface.
func (h *AuthSessionHandler) renderTextError(c *gin.Context, status int, msg string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(status, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>caw — login</title></head><body><main style="max-width:640px;margin:2rem auto;font-family:-apple-system,sans-serif"><h1>caw login</h1><p>%s</p></main></body></html>`, htmlAttrEscape(msg))
}

// githubOAuthURL builds the GitHub /login/oauth/authorize URL with state=
// session_id and redirect_uri set to our /auth/cb/github.
func (h *AuthSessionHandler) githubOAuthURL(clientID, sessionID string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("state", sessionID)
	q.Set("redirect_uri", h.cfg.BaseURL+"/auth/cb/github")
	return h.cfg.GithubBase + "/login/oauth/authorize?" + q.Encode()
}

// fireLoopback POSTs the JSON-encoded payload to the MCP's listener with a
// bounded timeout. All errors are logged and swallowed — a dead listener
// degrades to the MCP's own login-tool timeout (5 min).
func (h *AuthSessionHandler) fireLoopback(ctx context.Context, redirectURL string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("auth loopback: marshal payload: %v", err)
		return
	}
	subCtx, cancel := context.WithTimeout(ctx, authLoopbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(subCtx, http.MethodPost, redirectURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("auth loopback: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("auth loopback: POST %s: %v", redirectURL, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		log.Printf("auth loopback: POST %s returned status %d", redirectURL, resp.StatusCode)
	}
}

// validateLoopbackRedirect refuses anything that isn't an http://127.0.0.1:PORT/...
// or http://[::1]:PORT/... URL — the only places the MCP can legally bind a
// listener. Without this, a hostile /auth/start body could direct the picker-
// fire to a server the user does not control.
func validateLoopbackRedirect(raw string) error {
	if raw == "" {
		return fmt.Errorf("loopback_redirect required for mode=loopback")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("loopback_redirect not a URL: %w", err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("loopback_redirect must use scheme http (got %q)", u.Scheme)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return fmt.Errorf("loopback_redirect host must be 127.0.0.1, ::1, or localhost (got %q)", host)
	}
	if u.Port() == "" {
		return fmt.Errorf("loopback_redirect must include an explicit port")
	}
	return nil
}

// normalizeInstallIDs trims whitespace, drops empties, and de-dups while
// preserving order. The form layer is permissive (the picker form repeats
// installation_ids[] per checkbox).
func normalizeInstallIDs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// generateDeviceCode mints a 44-char base64url-encoded random secret. This is
// the value the MCP holds + posts to /auth/poll; never shown to a human.
func generateDeviceCode() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// 32 raw bytes → 43 base64url chars; matches the OAuth device-flow norms.
	return strings.TrimRight(base32.StdEncoding.EncodeToString(b[:]), "="), nil
}

// generateUserCode mints an 8-char human-typeable code in the form XXXX-XXXX
// using the userCodeAlphabet (no 0/O/1/I/L to keep monospace fonts honest).
func generateUserCode() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	chars := make([]byte, 8)
	for i := 0; i < 8; i++ {
		chars[i] = userCodeAlphabet[int(b[i])%len(userCodeAlphabet)]
	}
	return string(chars[:4]) + "-" + string(chars[4:]), nil
}
