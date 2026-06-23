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
	"io"
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
	"github.com/ravencloak-org/caw/internal/rebase"
	"github.com/ravencloak-org/caw/internal/repoaccess"
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
	if len(os.Args) > 1 && os.Args[1] == "revoke-token" {
		if err := revokeToken(st, os.Args[2:]); err != nil {
			log.Fatalf("revoke-token: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate-tokens" {
		if err := migrateTokens(st, os.Args[2:], os.Stdout); err != nil {
			log.Fatalf("migrate-tokens: %v", err)
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

	// Resolve the GitHub REST token source for the mergeability poll and
	// auto-merge: prefer GitHub App installation tokens (scoped per repo), then
	// a static PAT (CAW_GITHUB_TOKEN); leave both disabled if neither is set.
	var ghClient *ghclient.Client
	if tokenSrc := buildGitHubTokenSource(cfg, st); tokenSrc != nil {
		ghClient = ghclient.New(cfg.GitHubAPIBase, tokenSrc)
		opts = append(opts, settle.WithPoller(mergeability.New(ghClient)))
	} else {
		log.Println("warning: no GitHub App key or CAW_GITHUB_TOKEN; mergeability poll + auto-merge disabled")
	}

	// Wire orphan rebase handler (Slice 6, ADR-0002/0005).
	// Use cfg.AppID as a stable holder identity; fall back to "hub-orphan".
	holderID := cfg.AppID
	if holderID == "" {
		holderID = "hub-orphan"
	}
	// The git working directory for orphan rebases defaults to the process cwd;
	// operators can set CAW_REBASE_DIR to override (e.g. a shared workspace).
	rebaseDir := os.Getenv("CAW_REBASE_DIR")
	orphanRunner := rebase.NewExecRunner(rebaseDir)
	// Pass ghClient as rebase.AutoMerger only when it is non-nil; passing a typed
	// nil would produce a non-nil interface with a nil pointer (runtime panic).
	var autoMerger rebase.AutoMerger
	if ghClient != nil {
		autoMerger = ghClient
	}
	orphanHandler := rebase.NewOrphanHandler(holderID, st, orphanRunner, autoMerger,
		rebase.WithLeaseTTL(cfg.RebaseLeaseTTLSeconds),
		rebase.WithOrphanHeartbeatInterval(cfg.RebaseHeartbeat),
	)
	opts = append(opts, settle.WithOrphanRebaseHandler(orphanHandler))

	engine := settle.New(st, sseHub, cfg.SettleGrace, opts...)

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

	// Build the install-callback handler (ADR-0010). It serves the App's
	// Setup URL after a user installs, mints a Watcher token on the spot, and
	// renders it once. It is independent of the manifest flow: as long as
	// BaseURL is set we register it; missing App credentials are reported by
	// the handler at request time.
	//
	// Credentials are resolved per request, env first then store, so a
	// hand-registered App (CAW_APP_CLIENT_ID/SECRET in env) works alongside a
	// manifest-registered App (creds in DB) without restart.
	var ich *hub.InstallCallbackHandler
	var ash *hub.AuthSessionHandler
	if cfg.BaseURL != "" {
		credsFn := func() (string, string, bool, error) {
			if cfg.AppClientID != "" && cfg.AppClientSecret != "" {
				return cfg.AppClientID, cfg.AppClientSecret, true, nil
			}
			creds, ok, err := st.LoadAppCredentials()
			if err != nil {
				return "", "", false, err
			}
			if !ok || creds.ClientID == "" || creds.ClientSecret == "" {
				return "", "", false, nil
			}
			return creds.ClientID, creds.ClientSecret, true, nil
		}
		ich, err = hub.NewInstallCallbackHandler(hub.InstallCallbackConfig{
			BaseURL:      cfg.BaseURL,
			MintFn:       mintFn,
			CredsFn:      credsFn,
			SessionStore: st,
		})
		if err != nil {
			log.Fatalf("install callback handler: %v", err)
		}
		// AppSlugFn: env CAW_APP_SLUG wins (operator override for the rare
		// case of a brand-new self-host with no installations yet), then
		// store.AnyAppSlug as the fallback (populated by the manifest flow
		// + installation.created webhook).
		appSlugFn := func() string {
			if cfg.AppSlug != "" {
				return cfg.AppSlug
			}
			s, err := st.AnyAppSlug()
			if err != nil {
				log.Printf("warning: AnyAppSlug: %v", err)
				return ""
			}
			return s
		}
		secureCookie := len(cfg.BaseURL) >= 8 && cfg.BaseURL[:8] == "https://"
		ash, err = hub.NewAuthSessionHandler(hub.AuthSessionHandlerConfig{
			BaseURL:      cfg.BaseURL,
			Store:        st,
			MintFn:       mintFn,
			CredsFn:      credsFn,
			AppSlugFn:    appSlugFn,
			SecureCookie: secureCookie,
		})
		if err != nil {
			log.Fatalf("auth session handler: %v", err)
		}
	} else {
		log.Println("warning: CAW_BASE_URL empty; self-service install-callback + /auth/* routes disabled")
	}

	// Auth v2 Phase 2: per-user repo-access decision cache. The Checker
	// reuses the App's installation-token client (already built above)
	// to call /repos/{owner}/{repo}/collaborators/{username}/permission.
	// When the App is not configured (no PEM, no manifest creds yet), the
	// checker is nil — the cache then fails closed on every user-bound
	// lookup, which is correct: an unconfigured Hub cannot authorize.
	var repoChecker repoaccess.Checker
	if itc := buildInstallTokenClient(cfg, st); itc != nil {
		repoChecker = repoaccess.NewHTTPChecker(cfg.GitHubAPIBase, itc.Token, nil)
	} else {
		log.Println("warning: GitHub App credentials missing; per-user repo-access checks will fail closed for user-bound tokens")
	}
	repoCache := repoaccess.NewCache(repoChecker, repoaccess.Options{})

	// Auth-v2 Phase 3.5 (issue #60): per-user control-stream fan-out hub.
	// One instance per process — server.New wires the route, hub ingest wires
	// the publish side via controlPublisherAdapter.
	controlHub := sse.NewControlHub()

	meh := hub.NewMeHandler(st, repoCache, nil)

	// Auth-v2 Phase 5 cutover. By default RequireRepoAccess rejects legacy
	// (NULL github_user_id) tokens with a 400 + actionable login URL. The
	// CAW_ALLOW_LEGACY_TOKENS=1 escape hatch preserves the pre-cutover
	// bypass for one more release of headroom so operators can run
	// `hub migrate-tokens` and ask their users to re-login. Read once at
	// startup so a flip needs a process restart (and shows up in the
	// startup log line).
	allowLegacyTokens := os.Getenv("CAW_ALLOW_LEGACY_TOKENS") == "1"
	if allowLegacyTokens {
		log.Println("warning: CAW_ALLOW_LEGACY_TOKENS=1 — legacy tokens bypass repo-access checks; remove this for the next release after all watchers re-login")
	}
	r := server.New(st, sseHub, controlHub, engine, []byte(cfg.GitHubWebhookSecret), mh, ich, ash, mintFn, repoCache, meh, allowLegacyTokens,
		server.WithLeaseTTL(cfg.RebaseLeaseTTLSeconds))

	// Auth v2 Phase 3: 15-min purger sweeps expired auth_sessions rows. The
	// rows are also rejected on read by the handlers' own expiry checks; the
	// purger just keeps the table small.
	purgerCtx, stopPurger := context.WithCancel(context.Background())
	defer stopPurger()
	go runAuthSessionPurger(purgerCtx, st)

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
	// Auth v2 Phase 2: start the cache sweeper now that we have a cancellable
	// context tied to the process lifetime.
	repoCache.Start(ctx)
	<-ctx.Done()

	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// runAuthSessionPurger runs every 15 min and deletes auth_sessions rows past
// their expires_at. Auth v2 Phase 3 — a defensive sweep, not a primary expiry
// gate (handlers also reject expired rows on read).
func runAuthSessionPurger(ctx context.Context, st *store.Store) {
	tick := time.NewTicker(15 * time.Minute)
	defer tick.Stop()
	// Run once immediately so a long-uptime hub doesn't carry crud across
	// restarts at the cost of the first tick interval.
	purgeOnce(st)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			purgeOnce(st)
		}
	}
}

func purgeOnce(st *store.Store) {
	n, err := st.DeleteExpiredAuthSessions(time.Now().Unix())
	if err != nil {
		log.Printf("auth session purger: %v", err)
		return
	}
	if n > 0 {
		log.Printf("auth session purger: deleted %d expired rows", n)
	}
}

// buildMintFn returns the function used to mint Hub installation tokens.
// It calls auth.GenerateToken + auth.GenerateID, persists the row (hash + id
// + lifecycle metadata), and returns the raw token (shown once) and the
// persisted token id (for revoke / list / audit).
//
// Phase 1 callers still pass userID=0 (legacy semantics): install-callback
// flows through "legacy", webhook auto-mint through "installation-auto",
// manifest setup through "manifest-setup". Phase 3+'s /auth/picker handler
// will start passing real github_user_id values.
func buildMintFn(st *store.Store) hub.MintFunc {
	return func(installationID, org, deviceLabel string, userID int64, userLogin string) (string, string, error) {
		raw, hash, err := auth.GenerateToken()
		if err != nil {
			return "", "", fmt.Errorf("buildMintFn GenerateToken: %w", err)
		}
		id, err := auth.GenerateID()
		if err != nil {
			return "", "", fmt.Errorf("buildMintFn GenerateID: %w", err)
		}
		t := store.Token{
			ID:              id,
			Hash:            hash,
			InstallationID:  installationID,
			Org:             org,
			GitHubUserLogin: userLogin,
			DeviceLabel:     deviceLabel,
		}
		if userID > 0 {
			t.GitHubUserID = &userID
		}
		if err := st.InsertTokenRow(t); err != nil {
			return "", "", fmt.Errorf("buildMintFn InsertTokenRow: %w", err)
		}
		return raw, id, nil
	}
}

// buildGitHubTokenSource resolves how the Hub authenticates its outbound GitHub
// REST calls (the mergeability poll + auto-merge). It prefers GitHub App
// installation tokens (minted per repo's installation and auto-refreshed), then
// a static PAT (CAW_GITHUB_TOKEN). Returns nil when neither is configured.
func buildGitHubTokenSource(cfg config.Config, st *store.Store) ghclient.TokenSource {
	if itc := buildInstallTokenClient(cfg, st); itc != nil {
		log.Println("GitHub App installation tokens enabled for mergeability poll + auto-merge")
		return func(ctx context.Context, owner, repo string) (string, error) {
			instID, ok, err := st.InstallationForRepo(owner + "/" + repo)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("no GitHub App installation for %s/%s", owner, repo)
			}
			return itc.Token(ctx, instID)
		}
	}
	if cfg.GitHubToken != "" {
		log.Println("using CAW_GITHUB_TOKEN (PAT) for mergeability poll + auto-merge")
		return ghclient.StaticToken(cfg.GitHubToken)
	}
	return nil
}

// buildInstallTokenClient constructs an InstallationTokenClient from GitHub App
// credentials, sourced from the environment (CAW_APP_ID + private key) or, for
// any missing piece, from the manifest-stored credentials in the database.
// Returns nil when no App id + private key are available.
func buildInstallTokenClient(cfg config.Config, st *store.Store) *githubapp.InstallationTokenClient {
	appID := cfg.AppID
	var pem []byte
	if cfg.AppPrivateKeyPEM != "" || cfg.AppPrivateKeyPath != "" {
		b, err := loadPEM(cfg)
		if err != nil {
			log.Printf("warning: GitHub App PEM unavailable: %v", err)
		} else {
			pem = b
		}
	}
	// Fall back to manifest-stored credentials for any missing piece.
	if appID == "" || len(pem) == 0 {
		if creds, ok, err := st.LoadAppCredentials(); err == nil && ok {
			if appID == "" {
				appID = creds.AppID
			}
			if len(pem) == 0 && creds.PEM != "" {
				pem = []byte(creds.PEM)
			}
		}
	}
	if appID == "" || len(pem) == 0 {
		return nil
	}

	signer, err := githubapp.NewAppJWTSigner(appID, pem)
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
	raw, _, err := buildMintFn(st)(args[0], org, "cli-mint-token", 0, "")
	if err != nil {
		return err
	}
	fmt.Println(raw)
	return nil
}

// revokeToken is the operator break-glass counterpart to mintToken: revoke a
// single token by id. Idempotent — re-revoking an already-revoked id is a
// silent success. Print "revoked <id>" so a wrapping shell pipeline can grep
// for confirmation. Args: <token_id>.
func revokeToken(st *store.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hub revoke-token <token_id>")
	}
	if err := st.RevokeToken(args[0], time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("revoked %s\n", args[0])
	return nil
}

// migrateTokens revokes every active legacy (NULL github_user_id) token row
// — the Auth v2 Phase 5 cutover companion to flipping RequireRepoAccess from
// "bypass" to "reject". Iterates store.ListLegacyTokens and calls RevokeToken
// on each row at one shared wall-clock second, so every legacy device's last
// authenticated request lands at the same instant in the audit log.
//
// Output: one human-grep-able line per row, then a "Revoked N legacy tokens"
// (or "would revoke N" under --dry-run) summary. Idempotent — a freshly
// revoked row drops out of the next invocation's list. Exit code 0 on
// success regardless of N; the zero-row case is the normal steady state
// post-migration.
//
// Args: [--dry-run]
func migrateTokens(st *store.Store, args []string, stdout io.Writer) error {
	dryRun := false
	for _, a := range args {
		switch a {
		case "--dry-run", "-n":
			dryRun = true
		case "-h", "--help":
			if _, err := fmt.Fprintln(stdout, "usage: hub migrate-tokens [--dry-run]"); err != nil {
				return fmt.Errorf("write help: %w", err)
			}
			return nil
		default:
			return fmt.Errorf("usage: hub migrate-tokens [--dry-run]: unknown arg %q", a)
		}
	}

	now := time.Now().Unix()
	rows, err := st.ListLegacyTokens(now)
	if err != nil {
		return fmt.Errorf("list legacy tokens: %w", err)
	}

	verb := "Revoked"
	if dryRun {
		verb = "Would revoke"
	}
	for _, t := range rows {
		if !dryRun {
			if err := st.RevokeToken(t.ID, now); err != nil {
				return fmt.Errorf("revoke %s: %w", t.ID, err)
			}
		}
		if _, err := fmt.Fprintf(stdout, "  token_id=%s installation_id=%s org=%s device_label=%s\n",
			t.ID, t.InstallationID, t.Org, t.DeviceLabel); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if _, err := fmt.Fprintf(stdout, "%s %d legacy tokens\n", verb, len(rows)); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}
