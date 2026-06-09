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

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ravencloak-org/caw/internal/github"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/store"
)

// maxBody caps the webhook payload we read (GitHub's documented ceiling is 25 MiB).
const maxBody = 25 << 20

// Hub holds the dependencies for handling webhooks and serving pending items.
type Hub struct {
	store   *store.Store
	secret  []byte
	settler *settle.Engine                                   // may be nil (e.g. in unit tests of pure ingest)
	mintFn  func(installationID, org string) (string, error) // nil → no auto-minting
}

// New constructs a Hub. settler may be nil, in which case settles are not scheduled.
func New(st *store.Store, secret []byte, settler *settle.Engine) *Hub {
	return &Hub{store: st, secret: secret, settler: settler}
}

// WithMintFunc sets the function called to mint a Hub token when an
// installation "created" event is received. Returns Hub for chaining.
func (h *Hub) WithMintFunc(fn func(installationID, org string) (string, error)) *Hub {
	h.mintFn = fn
	return h
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

	sigOK := len(h.secret) > 0 && VerifySignature(h.secret, body, c.GetHeader("X-Hub-Signature-256"))
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
			if _, err := h.mintFn(installID, org); err != nil {
				// Non-fatal: log but do not fail the webhook; token can be minted later.
				log.Printf("mint token for installation %s: %v", installID, err)
			}
		}

	case "deleted":
		if err := h.store.DeleteInstallation(installID); err != nil {
			return fmt.Errorf("handleInstallation delete: %w", err)
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
	}
	return nil
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

// HandlePending serves GET /pending: all current pending items (ADR-0006).
// The consumer decides which to act on; each item carries its SHA, PR state,
// and timestamp for relevance filtering.
func (h *Hub) HandlePending(c *gin.Context) {
	items, err := h.store.ListPending()
	if err != nil {
		log.Printf("list pending: %v", err)
		c.String(http.StatusInternalServerError, "pending")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}
