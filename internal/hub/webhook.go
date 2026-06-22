// Package hub implements the Caw Hub's HTTP-facing logic: webhook ingest,
// signature verification, delivery dedupe, Round bucketing, signal extraction,
// and the get_pending endpoint.
package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/github"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/store"
)

// maxBody caps the webhook payload we read (GitHub's documented ceiling is 25 MiB).
const maxBody = 25 << 20

// CacheFlusher is the subset of *repoaccess.Cache that webhook ingest calls
// to invalidate per-user access decisions when GitHub tells us an installation
// lost a repo (or the whole installation was removed). The Hub depends on the
// interface — not the concrete type — so unit tests can plug in a fake and
// the repoaccess import stays scoped to wiring (server + cmd/hub).
type CacheFlusher interface {
	// FlushRepo drops every cache entry for (installationID, fullName)
	// regardless of user. Called from installation_repositories.removed.
	FlushRepo(installationID, fullName string)
	// FlushInstallation drops every cache entry for installationID. Called
	// from installation.deleted.
	FlushInstallation(installationID string)
}

// ControlPublisher is the subset of *sse.ControlHub that webhook ingest calls
// to fan auth-v2 Phase 3.5 control-stream events (pr_opened / pr_closed /
// installation_added) out to the live MCP plugins of one github_user_id.
// Same indirection pattern as CacheFlusher: the Hub depends on the interface,
// not on the concrete type, so test files plug a fake and the sse package
// import stays scoped to wiring (server + cmd/hub).
type ControlPublisher interface {
	// Publish delivers a control event to every live subscriber of userID.
	// Returns the number of subscribers reached; zero is a no-op.
	Publish(userID int64, name string, data []byte) int
}

// Hub holds the dependencies for handling webhooks and serving pending items.
type Hub struct {
	store    *store.Store
	secret   []byte
	settler  *settle.Engine   // may be nil (e.g. in unit tests of pure ingest)
	mintFn   MintFunc         // nil → no auto-minting
	flusher  CacheFlusher     // nil → no Auth v2 cache invalidation (legacy / tests)
	control  ControlPublisher // nil → no Phase 3.5 control-stream fan-out
	nowFn    func() int64     // injectable clock for the active-token filter (tests)
	leaseTTL int64            // seconds granted per lease acquire/renew; defaults to leaseTTL const
}

// New constructs a Hub. settler may be nil, in which case settles are not scheduled.
// The lease TTL defaults to the leaseTTL constant; override it with WithLeaseTTL.
func New(st *store.Store, secret []byte, settler *settle.Engine) *Hub {
	return &Hub{store: st, secret: secret, settler: settler, leaseTTL: leaseTTL}
}

// WithLeaseTTL overrides the per-acquire/renew lease TTL (in seconds). A
// non-positive value is ignored, leaving the default leaseTTL in place. Returns
// Hub for chaining.
func (h *Hub) WithLeaseTTL(seconds int64) *Hub {
	if seconds > 0 {
		h.leaseTTL = seconds
	}
	return h
}

// WithMintFunc sets the function called to mint a Hub token when an
// installation "created" event is received. Returns Hub for chaining.
func (h *Hub) WithMintFunc(fn MintFunc) *Hub {
	h.mintFn = fn
	return h
}

// WithCacheFlusher sets the per-user repo-access cache to invalidate on
// installation / installation_repositories webhook events. Returns Hub for
// chaining. Safe to omit; nil-flusher Hubs simply skip the side effect.
func (h *Hub) WithCacheFlusher(f CacheFlusher) *Hub {
	h.flusher = f
	return h
}

// WithControlPublisher sets the auth-v2 Phase 3.5 control-stream publisher
// the webhook ingest calls when a pr_opened / pr_closed / installation_added
// event needs to fan out to every live MCP plugin of the matching
// github_user_id. Safe to omit; nil-publisher Hubs simply skip the side effect.
// Returns Hub for chaining.
func (h *Hub) WithControlPublisher(cp ControlPublisher) *Hub {
	h.control = cp
	return h
}

// now reads from the injected clock (test hook) when set, otherwise wall-clock.
// The active-token filter (revoked_at IS NULL AND expires_at > now) needs a
// deterministic clock in tests so a frozen "now" can prove an expired token
// is correctly skipped.
func (h *Hub) now() int64 {
	if h.nowFn != nil {
		return h.nowFn()
	}
	return time.Now().Unix()
}

// effectiveWebhookSecret returns the secret used to verify webhook signatures.
// A GitHub App provisioned via the manifest flow (Slice 5) carries its own
// webhook secret in app_credentials; prefer it so App deliveries verify without
// the operator mirroring CAW_GH_WEBHOOK_SECRET into the App config. Fall back to
// the static secret (CAW_GH_WEBHOOK_SECRET) until App credentials exist. A store
// failure is propagated, not swallowed, so the caller fails closed rather than
// verifying under an unintended secret.
func (h *Hub) effectiveWebhookSecret() ([]byte, error) {
	creds, ok, err := h.store.LoadAppCredentials()
	if err != nil {
		return nil, fmt.Errorf("load app credentials: %w", err)
	}
	if ok && creds.WebhookSecret != "" {
		return []byte(creds.WebhookSecret), nil
	}
	return h.secret, nil
}

// VerifySignature reports whether sigHeader ("sha256=<hex>") is a valid
// HMAC-SHA256 of payload under secret. The comparison is constant-time.
func VerifySignature(secret, payload []byte, sigHeader string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	want, err := hex.DecodeString(sigHeader[len(prefix):])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hmac.Equal(want, mac.Sum(nil))
}

// HandleWebhook is the POST /webhooks/github handler: verify the signature,
// dedupe by delivery id, ingest, and only THEN record the delivery as processed.
//
// Recording after ingest (not before) is deliberate: if ingest fails we return
// 5xx with the delivery un-recorded, so GitHub's redelivery of the same id is
// reprocessed rather than silently dropped as a "duplicate". ingest is
// idempotent (upserts), so a redelivery race is harmless.
func (h *Hub) HandleWebhook(c *gin.Context) {
	tracer := otel.Tracer("caw-hub/webhook")
	ctx, span := tracer.Start(c.Request.Context(), "webhook.handle")
	defer span.End()

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody))
	if err != nil {
		span.SetStatus(codes.Error, "read body")
		c.String(http.StatusBadRequest, "read body")
		return
	}

	secret, err := h.effectiveWebhookSecret()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "load webhook secret")
		log.Printf("webhook secret: %v", err)
		c.String(http.StatusInternalServerError, "signature verification unavailable")
		return
	}
	sigOK := len(secret) > 0 && VerifySignature(secret, body, c.GetHeader("X-Hub-Signature-256"))
	span.SetAttributes(attribute.Bool("webhook.sig_ok", sigOK))
	if !sigOK {
		span.SetStatus(codes.Error, "invalid signature")
		c.String(http.StatusUnauthorized, "invalid signature")
		return
	}

	event := c.GetHeader("X-GitHub-Event")
	delivery := c.GetHeader("X-GitHub-Delivery")
	span.SetAttributes(
		attribute.String("webhook.event", event),
		attribute.String("webhook.delivery", delivery),
	)

	if delivery != "" {
		seen, err := h.store.HasDelivery(delivery)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "dedupe check")
			log.Printf("dedupe check %s: %v", delivery, err)
			c.String(http.StatusInternalServerError, "dedupe")
			return
		}
		if seen {
			span.SetAttributes(attribute.Bool("webhook.duplicate", true))
			c.String(http.StatusOK, "duplicate")
			return
		}
	}

	var env github.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse payload")
		c.String(http.StatusBadRequest, "parse payload")
		return
	}

	if err := h.ingest(ctx, event, env); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "ingest")
		log.Printf("ingest %s: %v", event, err)
		c.String(http.StatusInternalServerError, "ingest")
		return
	}

	if delivery != "" {
		if _, err := h.store.SeenDelivery(delivery, event); err != nil {
			// Non-fatal: a missed mark just risks one idempotent reprocess.
			log.Printf("record delivery %s: %v", delivery, err)
		}
	}
	c.String(http.StatusAccepted, "accepted")
}

// nonFailing check conclusions that should not raise a checks signal.
var passingConclusions = map[string]bool{"success": true, "neutral": true, "skipped": true}

// ingest records the Round, extracts any signal, and arms the settle timer.
func (h *Hub) ingest(ctx context.Context, event string, env github.Envelope) error {
	tracer := otel.Tracer("caw-hub/webhook")
	ctx, span := tracer.Start(ctx, "webhook.ingest")
	defer span.End()
	span.SetAttributes(attribute.String("webhook.event", event))
	// Installation events carry no repository context; handle them first.
	switch event {
	case "installation":
		return h.handleInstallation(env)
	case "installation_repositories":
		return h.handleInstallationRepositories(env)
	}

	owner := env.Repository.Owner.Login
	repo := env.Repository.Name
	if owner == "" || repo == "" {
		return nil
	}
	span.SetAttributes(
		attribute.String("github.owner", owner),
		attribute.String("github.repo", repo),
	)
	_ = ctx // ctx is available for future child spans

	switch event {
	case "check_suite":
		cs := env.CheckSuite
		if cs == nil || env.Action != "completed" || len(cs.PullRequests) == 0 {
			return nil
		}
		number, sha := cs.PullRequests[0].Number, cs.HeadSHA
		if err := h.store.RecordRound(owner, repo, number, sha); err != nil {
			return err
		}
		if !passingConclusions[cs.Conclusion] && cs.Conclusion != "" {
			source := cs.App.Name
			if source == "" {
				source = "checks"
			}
			if err := h.store.AddSignal(store.Signal{
				Owner: owner, Repo: repo, Number: number, SHA: sha,
				SignalType: "checks", Source: source,
				ExternalID: fmt.Sprintf("suite-%d", cs.ID),
				Severity:   cs.Conclusion, Body: cs.Conclusion,
			}); err != nil {
				return err
			}
		}
		h.touch(owner, repo, number, sha)

	case "pull_request":
		if env.PullRequest == nil || env.PullRequest.Head.SHA == "" {
			return nil
		}
		// Auth-v2 Phase 3.5: fan out pr_opened / pr_closed to the control
		// stream keyed on env.Sender.ID (the actor — usually identical to
		// env.PullRequest.User.ID, but actor wins per the plan's rationale).
		// h.publishPRControl no-ops when control is nil or the sender has
		// no live user-bound token.
		if env.Action == "opened" || env.Action == "closed" {
			h.publishPRControl(owner, repo, env)
		}
		return h.store.RecordRound(owner, repo, env.PullRequest.Number, env.PullRequest.Head.SHA)

	case "pull_request_review":
		if env.PullRequest == nil || env.Review == nil {
			return nil
		}
		return h.comment(owner, repo, env.PullRequest.Number, env.PullRequest.Head.SHA,
			env.Sender.Login, fmt.Sprintf("review-%d", env.Review.ID), env.Review.Body)

	case "pull_request_review_comment":
		if env.PullRequest == nil || env.Comment == nil {
			return nil
		}
		return h.comment(owner, repo, env.PullRequest.Number, env.PullRequest.Head.SHA,
			env.Sender.Login, fmt.Sprintf("rc-%d", env.Comment.ID), env.Comment.Body)

	case "issue_comment":
		if env.Issue == nil || env.Comment == nil {
			return nil
		}
		sha, ok, err := h.store.LatestRoundSHA(owner, repo, env.Issue.Number)
		if err != nil {
			return err
		}
		if !ok {
			return nil // no known Round yet; nothing to attach the comment to
		}
		return h.comment(owner, repo, env.Issue.Number, sha,
			env.Sender.Login, fmt.Sprintf("ic-%d", env.Comment.ID), env.Comment.Body)
	}
	return nil
}

// handleInstallation processes GitHub App installation created/deleted events.
func (h *Hub) handleInstallation(env github.Envelope) error {
	if env.Installation == nil {
		return nil
	}
	installID := strconv.FormatInt(env.Installation.ID, 10)
	org := env.Installation.Account.Login

	switch env.Action {
	case "created":
		if err := h.store.UpsertInstallation(installID, org); err != nil {
			return fmt.Errorf("handleInstallation upsert: %w", err)
		}
		for _, r := range env.Repositories {
			if err := h.store.AddInstallationRepo(installID, r.FullName); err != nil {
				return fmt.Errorf("handleInstallation add repo: %w", err)
			}
		}
		if h.mintFn != nil {
			// Phase 1 widens to the new MintFunc shape but keeps the legacy
			// "no user binding" semantics this path inherits from v0.1.x:
			// userID=0 and deviceLabel="installation-auto" so the row is
			// recognizable in audit logs and Phase 5 can safely sunset it.
			if _, _, err := h.mintFn(installID, org, "installation-auto", 0, ""); err != nil {
				// Non-fatal: log but do not fail the webhook; token can be minted later.
				log.Printf("mint token for installation %s: %v", installID, err)
			}
		}

	case "deleted":
		if err := h.store.DeleteInstallation(installID); err != nil {
			return fmt.Errorf("handleInstallation delete: %w", err)
		}
		// Auth v2 Phase 4 — defense in depth. Phase 2's cache flush below
		// drops in-memory allow decisions, but the tokens themselves must
		// also die at the persistence layer so a token can never re-allow
		// against a freshly re-installed App that happens to land on the
		// same installation_id later (vanishingly unlikely, but cheap to
		// foreclose). RevokeTokensForInstallation is idempotent; an error
		// is logged but not fatal — the webhook must still ack to GitHub.
		if n, err := h.store.RevokeTokensForInstallation(installID, time.Now().Unix()); err != nil {
			log.Printf("revoke tokens for deleted installation %s: %v", installID, err)
		} else if n > 0 {
			log.Printf("installation %s deleted: revoked %d token(s)", installID, n)
		}
		// Auth v2: drop every cached repo-access decision for this
		// installation. The installation is gone — no token under it
		// should still see an "allow" the cache might still hold.
		if h.flusher != nil {
			h.flusher.FlushInstallation(installID)
		}
	}
	return nil
}

// handleInstallationRepositories processes repositories added/removed events.
func (h *Hub) handleInstallationRepositories(env github.Envelope) error {
	if env.Installation == nil {
		return nil
	}
	installID := strconv.FormatInt(env.Installation.ID, 10)

	for _, r := range env.RepositoriesAdded {
		if err := h.store.AddInstallationRepo(installID, r.FullName); err != nil {
			return fmt.Errorf("handleInstallationRepositories add: %w", err)
		}
	}
	for _, r := range env.RepositoriesRemoved {
		if err := h.store.RemoveInstallationRepo(installID, r.FullName); err != nil {
			return fmt.Errorf("handleInstallationRepositories remove: %w", err)
		}
		// Auth v2: a repo leaving the installation must immediately
		// invalidate any positive cache entry for that (instID, repo)
		// regardless of user.
		if h.flusher != nil {
			h.flusher.FlushRepo(installID, r.FullName)
		}
	}
	// Auth-v2 Phase 3.5: notify every user with an active token on this
	// installation that repos were added — lets the MCP refresh its `/me`
	// cache without polling. We use env.Action so we only publish on the
	// "added" payload (GitHub fires the same event shape for both deltas).
	if env.Action == "added" && len(env.RepositoriesAdded) > 0 {
		h.publishInstallationAdded(installID, env.Installation.Account.Login)
	}
	return nil
}

// publishPRControl fans `pr_opened` / `pr_closed` out to every live
// control-stream subscriber of env.Sender.ID. The hub is the source of truth
// for "which device is the user holding" via active-token rows; the control
// stream itself only sees subscribers, so the lookup picks the user id once
// and the hub publish handles the per-device fan-out.
func (h *Hub) publishPRControl(owner, repo string, env github.Envelope) {
	if h.control == nil || env.Sender.ID == 0 || env.PullRequest == nil {
		return
	}
	tokens, err := h.store.TokensByGitHubUserID(env.Sender.ID, h.now())
	if err != nil {
		log.Printf("phase 3.5 fan-out: tokens by user %d: %v", env.Sender.ID, err)
		return
	}
	if len(tokens) == 0 {
		return // no live MCP token for this user; nothing to push to
	}
	name := "pr_opened"
	var payload []byte
	if env.Action == "closed" {
		name = "pr_closed"
		payload, err = json.Marshal(map[string]any{
			"owner": owner, "repo": repo, "number": env.PullRequest.Number,
		})
	} else {
		payload, err = json.Marshal(map[string]any{
			"owner":        owner,
			"repo":         repo,
			"number":       env.PullRequest.Number,
			"head_sha":     env.PullRequest.Head.SHA,
			"author_login": env.PullRequest.User.Login,
		})
	}
	if err != nil {
		log.Printf("phase 3.5 fan-out: marshal %s: %v", name, err)
		return
	}
	// Distinct users; tokens are scoped to the same sender ID by the
	// SQL filter above, so we only need ONE Publish — the control hub
	// itself fans across every live subscriber for that user id.
	h.control.Publish(env.Sender.ID, name, payload)
}

// publishInstallationAdded fans `installation_added` out to every user with
// an active token on installID — they may want to refresh /me.
func (h *Hub) publishInstallationAdded(installID, org string) {
	if h.control == nil {
		return
	}
	tokens, err := h.store.TokensForInstallation(installID, h.now())
	if err != nil {
		log.Printf("phase 3.5 fan-out: tokens for installation %s: %v", installID, err)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"installation_id": installID,
		"org":             org,
	})
	if err != nil {
		log.Printf("phase 3.5 fan-out: marshal installation_added: %v", err)
		return
	}
	// De-dup by github_user_id: one user with multiple devices still gets
	// ONE publish (the control hub itself fans to every device).
	seen := make(map[int64]struct{}, len(tokens))
	for _, t := range tokens {
		if t.GitHubUserID == nil || *t.GitHubUserID == 0 {
			continue // legacy / install-auto rows have no user binding
		}
		uid := *t.GitHubUserID
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		h.control.Publish(uid, "installation_added", payload)
	}
}

// comment records a comments-type signal for a Round and arms the settle timer.
func (h *Hub) comment(owner, repo string, number int, sha, source, externalID, body string) error {
	if sha == "" {
		return nil
	}
	if source == "" {
		source = "comment"
	}
	if err := h.store.RecordRound(owner, repo, number, sha); err != nil {
		return err
	}
	if err := h.store.AddSignal(store.Signal{
		Owner: owner, Repo: repo, Number: number, SHA: sha,
		SignalType: "comments", Source: source, ExternalID: externalID, Body: body,
	}); err != nil {
		return err
	}
	h.touch(owner, repo, number, sha)
	return nil
}

func (h *Hub) touch(owner, repo string, number int, sha string) {
	if h.settler != nil {
		h.settler.Touch(owner, repo, number, sha)
	}
}

// HandlePending serves GET /pending: pending items scoped to the caller's
// installation (ADR-0003, ADR-0006). The setup bootstrap token ("setup") is
// rejected because it is not associated with any real installation.
func (h *Hub) HandlePending(c *gin.Context) {
	installationID, _ := c.Get(auth.ContextInstallationID)
	holder, ok := installationID.(string)
	if !ok || holder == "" || holder == "setup" {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	items, err := h.store.ListPendingForInstallation(holder)
	if err != nil {
		log.Printf("list pending for installation %s: %v", holder, err)
		c.String(http.StatusInternalServerError, "pending")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
