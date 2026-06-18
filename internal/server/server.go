// Package server wires the Hub's HTTP routes onto a Gin engine.
package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	otelgin "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/hub"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

// New builds the Gin engine with all routes wired. The SSE and pending routes
// are gated by installation-token auth (ADR-0003); the SSE route is registered
// without buffering middleware so the held connection streams (ADR-0001).
//
// mh is optional: when non-nil the GitHub App Manifest flow routes are registered.
// mintFn is optional: when non-nil it is passed to the Hub so that installation
// "created" webhook events automatically mint a Hub token.
func New(st *store.Store, sseHub *sse.Hub, engine *settle.Engine, secret []byte, mh *hub.ManifestHandler, mintFn func(installationID, org string) (string, error)) *gin.Engine {
	r := gin.New()
	r.Use(otelgin.Middleware("caw-hub"), gin.Logger(), gin.Recovery())

	h := hub.New(st, secret, engine)
	if mintFn != nil {
		h.WithMintFunc(mintFn)
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

	authMW := auth.Required(st)
	scopeMW := hub.RequireRepoScope(st)
	r.GET("/pending", authMW, h.HandlePending)
	r.GET("/sse/:owner/:repo/:number", authMW, scopeMW, sseHub.Handler(sseKey))
	r.POST("/leases/:owner/:repo/:number", authMW, scopeMW, h.HandleAcquireLease)
	r.PUT("/leases/:owner/:repo/:number/heartbeat", authMW, scopeMW, h.HandleRenewLease)
	r.DELETE("/leases/:owner/:repo/:number", authMW, scopeMW, h.HandleReleaseLease)

	return r
}

// sseKey derives the PR subscription key from the request path; it must match
// settle.PRKey's format (owner/repo#number).
func sseKey(c *gin.Context) string {
	return fmt.Sprintf("%s/%s#%s", c.Param("owner"), c.Param("repo"), c.Param("number"))
}
