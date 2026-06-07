// Package server wires the Hub's HTTP routes onto a Gin engine.
package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/hub"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

// New builds the Gin engine with all routes wired. The SSE and pending routes
// are gated by installation-token auth (ADR-0003); the SSE route is registered
// without buffering middleware so the held connection streams (ADR-0001).
func New(st *store.Store, sseHub *sse.Hub, engine *settle.Engine, secret []byte) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	h := hub.New(st, secret, engine)
	r.POST("/webhooks/github", h.HandleWebhook)
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	authMW := auth.Required(st)
	r.GET("/pending", authMW, h.HandlePending)
	r.GET("/sse/:owner/:repo/:number", authMW, sseHub.Handler(sseKey))

	return r
}

// sseKey derives the PR subscription key from the request path; it must match
// settle.PRKey's format (owner/repo#number).
func sseKey(c *gin.Context) string {
	return fmt.Sprintf("%s/%s#%s", c.Param("owner"), c.Param("repo"), c.Param("number"))
}
