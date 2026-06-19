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
// /sse/..., /pending, /leases/... — legacy (NULL github_user_id) tokens bypass
// with a Deprecation header (Phase 2; enforcement starts in Phase 5). Tests
// that exercise only legacy tokens may pass repoaccess.NewCache(nil, …) and
// never reach the cache's checker seam.
func New(
	st *store.Store,
	sseHub *sse.Hub,
	engine *settle.Engine,
	secret []byte,
	mh *hub.ManifestHandler,
	ich *hub.InstallCallbackHandler,
	mintFn hub.MintFunc,
	repoAccess *repoaccess.Cache,
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

	authMW := auth.Required(st)
	scopeMW := hub.RequireRepoScope(st)
	accessMW := hub.RequireRepoAccess(repoAccess)
	r.GET("/pending", authMW, h.HandlePending)
	r.GET("/sse/:owner/:repo/:number", authMW, scopeMW, accessMW, sseHub.Handler(sseKey))
	r.POST("/leases/:owner/:repo/:number", authMW, scopeMW, accessMW, h.HandleAcquireLease)
	r.PUT("/leases/:owner/:repo/:number/heartbeat", authMW, scopeMW, accessMW, h.HandleRenewLease)
	r.DELETE("/leases/:owner/:repo/:number", authMW, scopeMW, accessMW, h.HandleReleaseLease)

	return r
}

// sseKey derives the PR subscription key from the request path; it must match
// settle.PRKey's format (owner/repo#number).
func sseKey(c *gin.Context) string {
	return fmt.Sprintf("%s/%s#%s", c.Param("owner"), c.Param("repo"), c.Param("number"))
}
