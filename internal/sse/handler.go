package sse

import (
	"io"

	"github.com/gin-gonic/gin"
)

// Handler returns a Gin handler that holds an SSE connection for the PR key
// (derived from the request by keyFn) until the client disconnects, streaming
// each published summary as a "summary" event.
//
// Buffering middleware must not wrap this route; the X-Accel-Buffering header
// also tells a fronting proxy not to buffer (ADR-0001).
func (h *Hub) Handler(keyFn func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := keyFn(c)
		sub := h.Subscribe(key)
		defer h.Unsubscribe(sub)

		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Writer.Flush()

		ctx := c.Request.Context()
		c.Stream(func(_ io.Writer) bool {
			select {
			case <-ctx.Done():
				return false
			case msg, ok := <-sub.C:
				if !ok {
					return false
				}
				c.SSEvent("summary", string(msg))
				return true
			}
		})
	}
}
