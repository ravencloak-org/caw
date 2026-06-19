// Package sse — control-stream HTTP handler for auth-v2 Phase 3.5 (issue #60).
package sse

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// pingInterval is how often the control handler emits an empty `ping` keepalive
// so a fronting proxy (Cloudflare's 100s idle ceiling) doesn't tear down a
// long-lived but silent user.
const pingInterval = 25 * time.Second

// ControlHandler returns a Gin handler for GET /sse/me/control. It reads the
// authenticated github_user_id via userIDFn (which auth.Required has already
// populated in the gin.Context) and holds an SSE stream open until the client
// disconnects, streaming each event published to the user's control channel.
//
// userIDFn returns the resolved id and an `ok` flag: zero / not-set MUST yield
// 400 with the actionable message — a legacy token reaching this route is a
// configuration bug (the user needs to re-login), not something the handler
// can silently degrade.
//
// Buffering middleware must not wrap this route; X-Accel-Buffering also tells
// the fronting proxy not to buffer (ADR-0001 / per-PR Handler discipline).
func (h *ControlHub) ControlHandler(userIDFn func(*gin.Context) (int64, bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := userIDFn(c)
		if !ok || uid == 0 {
			c.String(http.StatusBadRequest,
				"control stream requires a user-bound token; run `login` from your agent")
			return
		}

		sub := h.Subscribe(uid)
		defer h.Unsubscribe(sub)

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Writer.Flush()

		ctx := c.Request.Context()
		tick := time.NewTicker(pingInterval)
		defer tick.Stop()

		c.Stream(func(_ io.Writer) bool {
			select {
			case <-ctx.Done():
				return false
			case evt, open := <-sub.C:
				if !open {
					return false
				}
				c.SSEvent(evt.Name, string(evt.Data))
				return true
			case t := <-tick.C:
				c.SSEvent("ping", fmt.Sprintf(`{"ts":%d}`, t.Unix()))
				return true
			}
		})
	}
}
