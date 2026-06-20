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
	// legacy (pre-Auth v2, NULL github_user_id) token. Phase 2 honored
	// the request silently; Phase 5 rejects with 400 by default and only
	// emits this header when the operator opted in via
	// CAW_ALLOW_LEGACY_TOKENS=1 (RequireRepoAccessOptions.AllowLegacyTokens).
	DeprecationLegacyToken = "legacy-token"
	// DeprecationStaleAllow signals: GitHub was unavailable, the prior
	// positive cache entry within the 30-min grace window kept this
	// connection alive. Honor with caution — the decision is up to 30 min
	// old.
	DeprecationStaleAllow = "stale-allow"
)

// RequireRepoAccessOptions configures the middleware. Each field is read once
// at constructor time (cmd/hub/main.go owns the env-flag resolution) and
// captured by the returned closure, so a hot-path request never touches the
// environment.
type RequireRepoAccessOptions struct {
	// AllowLegacyTokens preserves the Phase 2 behavior: a token with
	// github_user_id == 0 (every v0.1.x prod token) is allowed through
	// with a `Deprecation: legacy-token` response header instead of being
	// rejected with 400. Operators flip CAW_ALLOW_LEGACY_TOKENS=1 to
	// extend the migration window by one more release; the default
	// (false) is the Phase 5 cutover behavior.
	AllowLegacyTokens bool
}

// RequireRepoAccess returns gin middleware that gates the request on whether
// the authenticated GitHub user actually has read access to the requested
// owner/repo. It MUST sit AFTER auth.Required (which sets the
// installation_id and github_user_id in the gin context) and AFTER
// RequireRepoScope (which validates the (installation, repo) tuple against
// the local DB and rejects obvious-mismatch tokens without a GitHub call).
//
// Behavior matrix (matches the Auth v2 plan §"Failure mode summary",
// updated for Phase 5):
//
//	Condition                                       Response
//	--------------------------------------------    ------------------------------------
//	Legacy token + AllowLegacyTokens=false          400 + JSON {"error": "user-bound token required", ...}
//	Legacy token + AllowLegacyTokens=true           200 / next + Deprecation: legacy-token
//	GitHub permission ≥ read                        200 / next
//	GitHub 404 (no access)                          404
//	Cache: positive entry within grace + GH 5xx     200 / next + Deprecation: stale-allow
//	GitHub 5xx with no usable prior cache entry     503 + Retry-After: 30
//	GitHub 403 (App permissions misconfigured)      500 (operator bug; logged)
//
// cache must be non-nil. A test harness that does not exercise user-bound
// tokens may pass repoaccess.NewCache(nil, …) — Lookup is never reached
// because every legacy lookup short-circuits here.
func RequireRepoAccess(cache *repoaccess.Cache, opts RequireRepoAccessOptions) gin.HandlerFunc {
	if cache == nil {
		panic("hub.RequireRepoAccess: cache is nil")
	}
	allowLegacy := opts.AllowLegacyTokens
	return func(c *gin.Context) {
		userIDRaw, _ := c.Get(auth.ContextGitHubUserID)
		userID, _ := userIDRaw.(int64)

		if userID == 0 {
			tokenID, _ := c.Get(auth.ContextTokenID)
			instID, _ := c.Get(auth.ContextInstallationID)
			if !allowLegacy {
				// Phase 5 cutover: legacy tokens are rejected with an
				// actionable 400 the MCP can surface to the user.
				// `login_url` is a relative path so it composes with
				// whatever public URL the hub is reached on.
				log.Printf(
					"auth-v2: legacy-token rejected method=%s path=%s installation=%v token_id=%v",
					c.Request.Method, c.FullPath(), instID, tokenID,
				)
				if span := trace.SpanFromContext(c.Request.Context()); span.IsRecording() {
					span.SetAttributes(
						attribute.Bool("caw.legacy_token", true),
						attribute.Bool("caw.legacy_token.rejected", true),
					)
				}
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error":     "user-bound token required",
					"login_url": "/auth/start",
					"message":   "user-bound token required; run `login` from your agent",
				})
				return
			}
			// Operator escape hatch (CAW_ALLOW_LEGACY_TOKENS=1): preserve
			// the Phase 2 bypass with a Deprecation header so the MCP /
			// operator dashboards surface "still on the old token shape"
			// for one more release.
			c.Header("Deprecation", DeprecationLegacyToken)
			log.Printf(
				"auth-v2: legacy-token bypass method=%s path=%s installation=%v token_id=%v (CAW_ALLOW_LEGACY_TOKENS=1)",
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
