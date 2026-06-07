// Command hub is the Caw Hub: it ingests GitHub webhooks, compiles PR feedback
// into summaries, and (in later slices) pushes them to Watchers over SSE.
//
// Slice 1 wires the HTTP server, webhook signature verification, delivery
// dedupe, Round bucketing, and the SQLite store.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/config"
	"github.com/ravencloak-org/caw/internal/hub"
	"github.com/ravencloak-org/caw/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if cfg.GitHubWebhookSecret == "" {
		log.Println("warning: CAW_GH_WEBHOOK_SECRET is empty; all webhooks will be rejected")
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	h := hub.New(st, []byte(cfg.GitHubWebhookSecret))
	r.POST("/webhooks/github", h.HandleWebhook)
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("hub listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	_ = os.Stdout.Sync()
}
