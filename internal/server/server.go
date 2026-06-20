// Package server wires the Hub's HTTP routes onto a Gin engine.
package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	otelgin "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/hub"
	"github.com/ravencloak-org/caw/internal/repoaccess"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

// New builds the Gin engine with all routes wired. The SSE and pending routes
// are gated by installation-token auth (ADR-0003); the SSE route is registered
// without buffering middleware so the held connection streams (ADR-0001).
//
// mh is optional: when non-nil the GitHub App Manifest flow routes are registered.
// ich is optional: when non-nil the App install Setup URL callback route is
// registered (ADR-0010 — self-service Watcher token issuance).
// mintFn is optional: when non-nil it is passed to the Hub so that installation
// "created" webhook events automatically mint a Hub token.
// repoAccess is required: it backs the Auth v2 RequireRepoAccess middleware on
// /sse/... and /leases/.... Phase 5 cutover: legacy (NULL github_user_id)
// tokens are rejected with 400 by default; allowLegacyTokens=true (operator's
// CAW_ALLOW_LEGACY_TOKENS=1 escape hatch) restores the pre-cutover bypass with
// a `Deprecation: legacy-token` header for one more release of headroom.
// /pending only uses the bearer auth (no per-repo path params; per-user
// filtering arrives with Phase 4's /me/* surface). Tests that exercise only
// legacy tokens may pass repoaccess.NewCache(nil, …) and never reach the
// cache's checker seam.
// meh is optional: when non-nil the Auth v2 Phase 4 /me/* routes are
// registered (per-user token management — list, revoke, panic-recover).
func New(
	st *store.Store,
	sseHub *sse.Hub,
	controlHub *sse.ControlHub,
	engine *settle.Engine,
	secret []byte,
	mh *hub.ManifestHandler,
	ich *hub.InstallCallbackHandler,
	ash *hub.AuthSessionHandler,
	mintFn hub.MintFunc,
	repoAccess *repoaccess.Cache,
	meh *hub.MeHandler,
	allowLegacyTokens bool,
) *gin.Engine {
	r := gin.New()
	r.Use(otelgin.Middleware("caw-hub"), gin.Logger(), gin.Recovery())

	h := hub.New(st, secret, engine)
	if mintFn != nil {
		h.WithMintFunc(mintFn)
	}
	if repoAccess != nil {
		// The Hub fan-outs cache invalidation from the relevant webhook
		// events (installation.deleted, installation_repositories.removed).
		h.WithCacheFlusher(repoAccess)
	}
	if controlHub != nil {
		// Auth-v2 Phase 3.5: webhook ingest fans `pr_opened` / `pr_closed`
		// and `installation_added` through this publisher.
		h.WithControlPublisher(controlPublisherAdapter{controlHub})
	}
	r.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", landingHTML)
	})
	r.POST("/webhooks/github", h.HandleWebhook)
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	if mh != nil {
		r.GET("/github/app/manifest", mh.HandleManifest)
		r.GET("/github/app/callback", mh.HandleCallback)
	}
	if ich != nil {
		r.GET("/github/app/install/callback", ich.Handle)
	}

	// Auth v2 Phase 3 /auth/* surface (issue #59). Distinct route group from
	// /github/app/install/callback — install_callback handles GitHub's setup
	// URL, /auth/* drives the MCP-initiated login wire protocol.
	if ash != nil {
		r.POST("/auth/start", ash.HandleStart)
		r.GET("/auth/start-help", ash.HandleStartHelp)
		r.GET("/auth/u/:session_id", ash.HandleBrowserStart)
		r.GET("/auth/cb/github", ash.HandleGithubCallback)
		r.GET("/auth/picker/:session_id", ash.HandlePickerGet)
		r.POST("/auth/picker/:session_id", ash.HandlePickerPost)
		r.GET("/auth/device", ash.HandleDevice)
		r.POST("/auth/poll", ash.HandlePoll)
		r.GET("/auth/done/:session_id", ash.HandleDone)
	}

	authMW := auth.Required(st)
	scopeMW := hub.RequireRepoScope(st)
	accessMW := hub.RequireRepoAccess(repoAccess, hub.RequireRepoAccessOptions{
		AllowLegacyTokens: allowLegacyTokens,
	})

	// Auth v2 Phase 4 /me/* surface (issue #61). All four routes are
	// auth-required only — they are per-user, not per-repo, so no
	// RequireRepoScope / RequireRepoAccess in the chain. Legacy tokens
	// (NULL github_user_id) are rejected by the handler itself.
	if meh != nil {
		r.GET("/me", authMW, meh.HandleMe)
		r.GET("/me/tokens", authMW, meh.HandleMeTokens)
		r.DELETE("/me/tokens/:id", authMW, meh.HandleMeTokenRevoke)
		r.POST("/me/recover", authMW, meh.HandleMeRecover)
	}

	r.GET("/pending", authMW, h.HandlePending)
	r.GET("/sse/:owner/:repo/:number", authMW, scopeMW, accessMW, sseHub.Handler(sseKey))
	r.POST("/leases/:owner/:repo/:number", authMW, scopeMW, accessMW, h.HandleAcquireLease)
	r.PUT("/leases/:owner/:repo/:number/heartbeat", authMW, scopeMW, accessMW, h.HandleRenewLease)
	r.DELETE("/leases/:owner/:repo/:number", authMW, scopeMW, accessMW, h.HandleReleaseLease)

	// Auth-v2 Phase 3.5 (issue #60): per-user control stream. auth.Required
	// ONLY — no RequireRepoScope / RequireRepoAccess because the stream is
	// keyed on the authenticated user, not a repo. Legacy (NULL github_user_id)
	// tokens are rejected at the handler with a 400 explaining the fix.
	if controlHub != nil {
		r.GET("/sse/me/control", authMW, controlHub.ControlHandler(controlUserIDFromContext))
	}

	return r
}

// sseKey derives the PR subscription key from the request path; it must match
// settle.PRKey's format (owner/repo#number).
func sseKey(c *gin.Context) string {
	return fmt.Sprintf("%s/%s#%s", c.Param("owner"), c.Param("repo"), c.Param("number"))
}

// controlPublisherAdapter adapts *sse.ControlHub to hub.ControlPublisher.
// The interface lives in `hub` so webhook ingest doesn't import `sse`; this
// glue keeps that boundary while letting cmd/hub wire the one concrete impl.
type controlPublisherAdapter struct{ hub *sse.ControlHub }

func (a controlPublisherAdapter) Publish(userID int64, name string, data []byte) int {
	return a.hub.Publish(userID, sse.ControlEvent{Name: name, Data: data})
}

// controlUserIDFromContext pulls the auth-v2 github_user_id out of the gin
// context that auth.Required populated. ok=false signals "legacy / unbound",
// which the control handler turns into a 400 + actionable message.
func controlUserIDFromContext(c *gin.Context) (int64, bool) {
	v, present := c.Get(auth.ContextGitHubUserID)
	if !present {
		return 0, false
	}
	id, ok := v.(int64)
	return id, ok && id != 0
}
