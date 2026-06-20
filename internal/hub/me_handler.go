// Package hub — Auth v2 Phase 4 token management surface (issue #61).
//
// Four routes, all gated by auth.Required ONLY (no per-repo middleware — these
// are per-user, not per-repo). Legacy tokens (rows with NULL github_user_id,
// surfaced as ContextGitHubUserID == 0) are rejected at the handler with
// `400 user-bound token required; run \`login\` from your agent` because the
// entire /me surface is meaningless without a user identity.
//
//	GET    /me           → user identity + installations the caller's tokens
//	                       hold + tokens_count. NEVER returns token hashes or
//	                       raw token values.
//	GET    /me/tokens    → list every token row owned by the caller; safe
//	                       metadata only (id, installation_id, org,
//	                       device_label, lifecycle timestamps). Revoked rows
//	                       are surfaced for audit; the consumer filters.
//	DELETE /me/tokens/:id → revoke one of the caller's tokens. Idempotent
//	                       (re-DELETE of an already-revoked id returns 204).
//	                       A token belonging to a different user returns 404
//	                       (NOT 403 — leaking "this id exists, just not yours"
//	                       confirms the id is real to a stranger holding only
//	                       a sibling token).
//	POST   /me/recover   → the panic button: revoke every token owned by the
//	                       caller in one transaction, plus flush the in-memory
//	                       repo-access cache for that user so any in-flight
//	                       allow decisions die immediately. 204 No Content +
//	                       Cache-Control: no-store. After this the caller's
//	                       MCP must run `login` again.
package hub

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

// UserCacheFlusher is the subset of *repoaccess.Cache that MeHandler calls
// from POST /me/recover. Kept as an interface here (rather than depending on
// the concrete *repoaccess.Cache) so the me_handler_test fakes the wiring
// without dragging repoaccess into the test build graph. May be nil — a
// Hub running without a repo-access cache simply skips the flush.
type UserCacheFlusher interface {
	FlushUser(userID int64)
}

// MeHandler holds the dependencies for the four /me routes.
type MeHandler struct {
	store   *store.Store
	flusher UserCacheFlusher // may be nil
	now     func() int64
}

// NewMeHandler constructs a MeHandler. flusher may be nil. The optional
// nowFn lets tests freeze time without monkey-patching time.Now; production
// callers pass nil and get time.Now().Unix() per call.
func NewMeHandler(st *store.Store, flusher UserCacheFlusher, nowFn func() int64) *MeHandler {
	if nowFn == nil {
		nowFn = func() int64 { return time.Now().Unix() }
	}
	return &MeHandler{store: st, flusher: flusher, now: nowFn}
}

// callerUserID reads the authenticated github_user_id from the gin context.
// Returns 0 for legacy tokens (the auth middleware writes 0 for NULL rows).
func callerUserID(c *gin.Context) int64 {
	if v, ok := c.Get(auth.ContextGitHubUserID); ok {
		if id, ok := v.(int64); ok {
			return id
		}
	}
	return 0
}

// rejectLegacyToken writes the canonical 400 response for the /me surface
// when the caller authenticated with a legacy (NULL github_user_id) token.
// Every /me handler aborts here before doing any work.
func rejectLegacyToken(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"error":     "user-bound token required",
		"login_url": "/auth/start",
		"message":   "user-bound token required; run `login` from your agent",
	})
}

// MeInstallation is one entry in GET /me's installations array. It is the
// installation portion of an active token row collapsed by installation_id,
// not a join into the installations table — a token's view of its
// installation is canonical for the purposes of /me ("which installations
// can I act in?").
type MeInstallation struct {
	InstallationID string `json:"installation_id"`
	Org            string `json:"org"`
}

// MeResponse is the JSON shape of GET /me. tokens_count surfaces the total
// number of token rows owned by the caller (including revoked ones) so a
// client can decide whether to render /me/tokens at all.
type MeResponse struct {
	GitHubUserID    int64            `json:"github_user_id"`
	GitHubUserLogin string           `json:"github_user_login"`
	Installations   []MeInstallation `json:"installations"`
	TokensCount     int              `json:"tokens_count"`
}

// HandleMe serves GET /me. Auth-required; legacy tokens rejected with 400.
func (h *MeHandler) HandleMe(c *gin.Context) {
	userID := callerUserID(c)
	if userID == 0 {
		rejectLegacyToken(c)
		return
	}

	rows, err := h.store.ListTokensForUser(userID)
	if err != nil {
		log.Printf("/me: list tokens for user %d: %v", userID, err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	// Login comes from the request context (set by auth.Required); if the
	// row store happened to have a richer value we honor that. Either is
	// the same string in practice — the user only owns one GitHub login.
	login, _ := c.Get(auth.ContextGitHubUserLogin)
	loginStr, _ := login.(string)

	// Collapse rows onto installation_id, keeping the first non-empty org
	// observed per installation. Tokens that are revoked still count as
	// "this user has historically held tokens for this installation" — but
	// only active rows enumerate Installations: a revoked-only entry is
	// hidden from the caller-facing surface (the count still surfaces it
	// via tokens_count + /me/tokens).
	seen := make(map[string]MeInstallation, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.RevokedAt != nil {
			continue
		}
		if _, ok := seen[r.InstallationID]; ok {
			continue
		}
		seen[r.InstallationID] = MeInstallation{
			InstallationID: r.InstallationID,
			Org:            r.Org,
		}
		order = append(order, r.InstallationID)
		// Backfill the login from a row if context is empty (defensive —
		// auth.Required always sets it for user-bound rows).
		if loginStr == "" && r.GitHubUserLogin != "" {
			loginStr = r.GitHubUserLogin
		}
	}
	installations := make([]MeInstallation, 0, len(order))
	for _, id := range order {
		installations = append(installations, seen[id])
	}

	c.JSON(http.StatusOK, MeResponse{
		GitHubUserID:    userID,
		GitHubUserLogin: loginStr,
		Installations:   installations,
		TokensCount:     len(rows),
	})
}

// TokenView is one row of GET /me/tokens. NEVER carries the token hash or the
// raw token value — only the lifecycle metadata the user needs to identify
// and revoke a row from the management UI.
type TokenView struct {
	TokenID        string `json:"token_id"`
	InstallationID string `json:"installation_id"`
	Org            string `json:"org,omitempty"`
	DeviceLabel    string `json:"device_label"`
	CreatedAt      int64  `json:"created_at"`
	LastUsedAt     *int64 `json:"last_used_at,omitempty"`
	ExpiresAt      *int64 `json:"expires_at,omitempty"`
	RevokedAt      *int64 `json:"revoked_at,omitempty"`
}

// MeTokensResponse is the JSON shape of GET /me/tokens.
type MeTokensResponse struct {
	Tokens []TokenView `json:"tokens"`
}

// HandleMeTokens serves GET /me/tokens. Auth-required; legacy tokens rejected.
// The response NEVER includes token hashes — those exist only in the DB and
// are useless to a client.
func (h *MeHandler) HandleMeTokens(c *gin.Context) {
	userID := callerUserID(c)
	if userID == 0 {
		rejectLegacyToken(c)
		return
	}

	rows, err := h.store.ListTokensForUser(userID)
	if err != nil {
		log.Printf("/me/tokens: list tokens for user %d: %v", userID, err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	views := make([]TokenView, 0, len(rows))
	for _, r := range rows {
		views = append(views, TokenView{
			TokenID:        r.ID,
			InstallationID: r.InstallationID,
			Org:            r.Org,
			DeviceLabel:    r.DeviceLabel,
			CreatedAt:      r.CreatedAt,
			LastUsedAt:     r.LastUsedAt,
			ExpiresAt:      r.ExpiresAt,
			RevokedAt:      r.RevokedAt,
		})
	}
	c.JSON(http.StatusOK, MeTokensResponse{Tokens: views})
}

// HandleMeTokenRevoke serves DELETE /me/tokens/:id. Auth-required; legacy
// tokens rejected. Idempotent: re-DELETE of an already-revoked id owned by
// the caller still returns 204. A token belonging to a different user
// returns 404 (NOT 403) — owner identity must not leak.
func (h *MeHandler) HandleMeTokenRevoke(c *gin.Context) {
	userID := callerUserID(c)
	if userID == 0 {
		rejectLegacyToken(c)
		return
	}
	tokenID := c.Param("id")
	if tokenID == "" {
		// Gin matches :id eagerly so this is mostly defensive; an empty
		// id maps to "not found" rather than 400 (consistent with cross-
		// user revocation — never surface why).
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	row, ok, err := h.store.GetTokenByID(tokenID)
	if err != nil {
		log.Printf("/me/tokens DELETE: get token %s: %v", tokenID, err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	// Two distinct "not yours" outcomes collapse onto 404:
	//   - id matches no row
	//   - id matches a row whose github_user_id != caller's
	// Returning 404 in both means a stranger holding a sibling token can't
	// confirm an id exists by probing.
	if !ok || row.GitHubUserID == nil || *row.GitHubUserID != userID {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if err := h.store.RevokeToken(tokenID, h.now()); err != nil {
		log.Printf("/me/tokens DELETE: revoke %s: %v", tokenID, err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusNoContent)
}

// HandleMeRecover serves POST /me/recover, the panic button. Revokes every
// token owned by the caller in one transaction, flushes the per-user repo-
// access cache (so any in-flight allow decisions die immediately), then 204.
// After this the caller's MCP MUST run `login` again — the very token used
// to authenticate this request was revoked by it.
//
// Cache-Control: no-store keeps any reverse proxy from caching the 204; this
// endpoint MUST always reach the hub.
func (h *MeHandler) HandleMeRecover(c *gin.Context) {
	userID := callerUserID(c)
	if userID == 0 {
		rejectLegacyToken(c)
		return
	}

	if _, err := h.store.RevokeAllTokensForUser(userID, h.now()); err != nil {
		log.Printf("/me/recover: revoke all for user %d: %v", userID, err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if h.flusher != nil {
		h.flusher.FlushUser(userID)
	}
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusNoContent)
}
