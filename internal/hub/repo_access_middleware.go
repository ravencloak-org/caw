package hub

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/repoaccess"
)

// Deprecation header values (Auth v2 plan §"Authorization model"). They are
// not RFC-9745-typed dates; the plan deliberately uses tag strings the MCP
// can match on string equality to surface a "your install is on a
// soon-to-die path" nudge.
const (
	// DeprecationLegacyToken signals: this request authenticated with a
	// legacy (pre-Auth v2, NULL github_user_id) token. Phase 2 honours
	// the request; Phase 5 will reject it.
	DeprecationLegacyToken = "legacy-token"
	// DeprecationStaleAllow signals: GitHub was unavailable, the prior
	// positive cache entry within the 30-min grace window kept this
	// connection alive. Honour with caution — the decision is up to 30 min
	// old.
	DeprecationStaleAllow = "stale-allow"
)

// RequireRepoAccess returns gin middleware that gates the request on whether
// the authenticated GitHub user actually has read access to the requested
// owner/repo. It MUST sit AFTER auth.Required (which sets the
// installation_id and github_user_id in the gin context) and AFTER
// RequireRepoScope (which validates the (installation, repo) tuple against
// the local DB and rejects obvious-mismatch tokens without a GitHub call).
//
// Behavior matrix (matches the Auth v2 plan §"Failure mode summary"):
//
//	Condition                                       Response
//	--------------------------------------------    ------------------------------------
//	Legacy token (github_user_id == 0)              200 / next + Deprecation: legacy-token
//	GitHub permission ≥ read                        200 / next
//	GitHub 404 (no access)                          404
//	Cache: positive entry within grace + GH 5xx     200 / next + Deprecation: stale-allow
//	GitHub 5xx with no usable prior cache entry     503 + Retry-After: 30
//	GitHub 403 (App permissions misconfigured)      500 (operator bug; logged)
//
// cache must be non-nil. A test harness that does not exercise user-bound
// tokens may pass repoaccess.NewCache(nil, …) — Lookup is never reached
// because every legacy lookup short-circuits here.
func RequireRepoAccess(cache *repoaccess.Cache) gin.HandlerFunc {
	if cache == nil {
		panic("hub.RequireRepoAccess: cache is nil")
	}
	return func(c *gin.Context) {
		userIDRaw, _ := c.Get(auth.ContextGitHubUserID)
		userID, _ := userIDRaw.(int64)

		if userID == 0 {
			// Legacy token: bypass with a Deprecation header so MCP /
			// operator dashboards can surface "still on the old token
			// shape" without us having to enforce yet (Phase 5 will).
			//
			// Single-line audit log per request so we can quantify the
			// legacy-token tail before Phase 5 turns this into a 401.
			c.Header("Deprecation", DeprecationLegacyToken)
			tokenID, _ := c.Get(auth.ContextTokenID)
			instID, _ := c.Get(auth.ContextInstallationID)
			log.Printf(
				"auth-v2: legacy-token bypass method=%s path=%s installation=%v token_id=%v",
				c.Request.Method, c.FullPath(), instID, tokenID,
			)
			if span := trace.SpanFromContext(c.Request.Context()); span.IsRecording() {
				span.SetAttributes(attribute.Bool("caw.legacy_token", true))
			}
			c.Next()
			return
		}

		installationID, _ := c.Get(auth.ContextInstallationID)
		instID, _ := installationID.(string)
		userLogin, _ := c.Get(auth.ContextGitHubUserLogin)
		login, _ := userLogin.(string)
		owner := c.Param("owner")
		repo := c.Param("repo")

		allowed, source, err := cache.Lookup(c.Request.Context(), instID, userID, login, owner, repo)
		if err != nil {
			if errors.Is(err, repoaccess.ErrConfigError) {
				log.Printf(
					"auth-v2: repoaccess config error owner=%s repo=%s user=%s install=%s err=%v",
					owner, repo, login, instID, err,
				)
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
			// Treat anything else (ErrUnavailable, context cancel, etc.)
			// as "GitHub is sad" — fail-closed with a retry hint.
			c.Header("Retry-After", "30")
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			// 404 not 403: do not confirm repo existence to a non-member.
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "repository not found or access denied",
			})
			return
		}
		if source == repoaccess.SourceStale {
			if span := trace.SpanFromContext(c.Request.Context()); span.IsRecording() {
				span.SetAttributes(attribute.Bool("caw.repo_access.stale", true))
			}
			c.Header("Deprecation", DeprecationStaleAllow)
		}
		c.Next()
	}
}
