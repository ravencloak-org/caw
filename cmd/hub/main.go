// Command hub is the Caw Hub: it ingests GitHub webhooks, compiles PR feedback
// into summaries, fans them out to Watchers over SSE, and stores orphaned
// feedback as pending items.
//
// Subcommand: `hub mint-token <installation_id> [org]` prints a new raw token.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/config"
	"github.com/ravencloak-org/caw/internal/ghclient"
	"github.com/ravencloak-org/caw/internal/mergeability"
	"github.com/ravencloak-org/caw/internal/server"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	if len(os.Args) > 1 && os.Args[1] == "mint-token" {
		if err := mintToken(st, os.Args[2:]); err != nil {
			log.Fatalf("mint-token: %v", err)
		}
		return
	}

	if cfg.GitHubWebhookSecret == "" {
		log.Println("warning: CAW_GH_WEBHOOK_SECRET is empty; all webhooks will be rejected")
	}

	sseHub := sse.New()
	var opts []settle.Option
	if cfg.GitHubToken != "" {
		poller := mergeability.New(ghclient.New(cfg.GitHubAPIBase, cfg.GitHubToken))
		opts = append(opts, settle.WithPoller(poller))
	} else {
		log.Println("warning: CAW_GITHUB_TOKEN is empty; mergeability polling disabled")
	}
	engine := settle.New(st, sseHub, settle.DefaultGrace, opts...)
	r := server.New(st, sseHub, engine, []byte(cfg.GitHubWebhookSecret))

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
}

// mintToken creates and stores an installation token, printing the raw value
// (shown once). Args: <installation_id> [org].
func mintToken(st *store.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hub mint-token <installation_id> [org]")
	}
	org := ""
	if len(args) > 1 {
		org = args[1]
	}
	raw, hash, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	if err := st.InsertToken(hash, args[0], org); err != nil {
		return err
	}
	fmt.Println(raw)
	return nil
}
