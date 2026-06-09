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
	"github.com/ravencloak-org/caw/internal/githubapp"
	"github.com/ravencloak-org/caw/internal/hub"
	"github.com/ravencloak-org/caw/internal/mergeability"
	"github.com/ravencloak-org/caw/internal/observability"
	"github.com/ravencloak-org/caw/internal/server"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

func main() {
	cfg := config.Load()

	shutdownObs, err := observability.Init(context.Background(), cfg)
	if err != nil {
		log.Fatalf("observability init: %v", err)
	}
	defer func() {
		if err := shutdownObs(context.Background()); err != nil {
			log.Printf("observability shutdown: %v", err)
		}
	}()

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

	// Build the Hub token mint function used by webhook-triggered install events
	// and the manifest callback handler.
	mintFn := buildMintFn(st)

	sseHub := sse.New()
	var opts []settle.Option
	if cfg.GitHubToken != "" {
		poller := mergeability.New(ghclient.New(cfg.GitHubAPIBase, cfg.GitHubToken))
		opts = append(opts, settle.WithPoller(poller))
	} else {
		log.Println("warning: CAW_GITHUB_TOKEN is empty; mergeability polling disabled")
	}
	engine := settle.New(st, sseHub, settle.DefaultGrace, opts...)

	// Build the GitHub App manifest handler. It is optional and gated: it
	// requires both CAW_BASE_URL and the operator bootstrap secret
	// (CAW_BOOTSTRAP_TOKEN). Without the bootstrap token the credential-minting
	// routes stay disabled rather than running unauthenticated.
	var mh *hub.ManifestHandler
	switch {
	case cfg.BaseURL != "" && cfg.BootstrapToken != "":
		mh, err = hub.NewManifestHandler(hub.ManifestConfig{
			BaseURL:          cfg.BaseURL,
			Store:            st,
			MintFn:           mintFn,
			BootstrapToken:   cfg.BootstrapToken,
			AllowRebootstrap: cfg.AllowRebootstrap,
		})
		if err != nil {
			log.Fatalf("manifest handler: %v", err)
		}
	case cfg.BaseURL != "":
		log.Println("warning: CAW_BASE_URL set but CAW_BOOTSTRAP_TOKEN empty; GitHub App manifest flow disabled")
	}

	r := server.New(st, sseHub, engine, []byte(cfg.GitHubWebhookSecret), mh, mintFn)

	// Build the installation token client if GitHub App credentials are configured.
	_ = buildInstallTokenClient(cfg)

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

// buildMintFn returns the function used to mint Hub installation tokens.
// It calls auth.GenerateToken, stores the hash, and returns the raw token.
// The raw token is never logged (security: keep secrets out of logs).
func buildMintFn(st *store.Store) func(installationID, org string) (string, error) {
	return func(installationID, org string) (string, error) {
		raw, hash, err := auth.GenerateToken()
		if err != nil {
			return "", fmt.Errorf("buildMintFn GenerateToken: %w", err)
		}
		if err := st.InsertToken(hash, installationID, org); err != nil {
			return "", fmt.Errorf("buildMintFn InsertToken: %w", err)
		}
		return raw, nil
	}
}

// buildInstallTokenClient constructs an InstallationTokenClient if the app
// credentials are configured. Returns nil when credentials are absent.
func buildInstallTokenClient(cfg config.Config) *githubapp.InstallationTokenClient {
	if cfg.AppID == "" {
		return nil
	}

	pemBytes, err := loadPEM(cfg)
	if err != nil {
		log.Printf("warning: GitHub App PEM unavailable: %v; installation token client disabled", err)
		return nil
	}

	signer, err := githubapp.NewAppJWTSigner(cfg.AppID, pemBytes)
	if err != nil {
		log.Printf("warning: NewAppJWTSigner: %v; installation token client disabled", err)
		return nil
	}

	apiBase := cfg.GitHubAPIBase
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return githubapp.NewInstallationTokenClient(signer, apiBase)
}

// loadPEM returns the RSA private key PEM bytes from either the inline config
// value (CAW_APP_PRIVATE_KEY_PEM) or by reading the file at
// CAW_APP_PRIVATE_KEY_PATH.
func loadPEM(cfg config.Config) ([]byte, error) {
	if cfg.AppPrivateKeyPEM != "" {
		return []byte(cfg.AppPrivateKeyPEM), nil
	}
	if cfg.AppPrivateKeyPath != "" {
		b, err := os.ReadFile(cfg.AppPrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read PEM file %q: %w", cfg.AppPrivateKeyPath, err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("neither CAW_APP_PRIVATE_KEY_PEM nor CAW_APP_PRIVATE_KEY_PATH is set")
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
