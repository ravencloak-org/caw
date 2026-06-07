// Package hub implements the Caw Hub's HTTP-facing logic: webhook ingest,
// signature verification, delivery dedupe, and Round bucketing.
package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/github"
	"github.com/ravencloak-org/caw/internal/store"
)

// maxBody caps the webhook payload we read (GitHub's documented ceiling is 25 MiB).
const maxBody = 25 << 20

// Hub holds the dependencies for handling webhooks.
type Hub struct {
	store  *store.Store
	secret []byte
}

// New constructs a Hub backed by st, verifying signatures with secret.
func New(st *store.Store, secret []byte) *Hub {
	return &Hub{store: st, secret: secret}
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

// HandleWebhook is the POST /webhooks/github handler. It verifies the
// signature, dedupes by delivery id, then buckets the event into a Round.
func (h *Hub) HandleWebhook(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody))
	if err != nil {
		c.String(http.StatusBadRequest, "read body")
		return
	}

	if len(h.secret) == 0 || !VerifySignature(h.secret, body, c.GetHeader("X-Hub-Signature-256")) {
		c.String(http.StatusUnauthorized, "invalid signature")
		return
	}

	event := c.GetHeader("X-GitHub-Event")
	if delivery := c.GetHeader("X-GitHub-Delivery"); delivery != "" {
		isNew, err := h.store.SeenDelivery(delivery, event)
		if err != nil {
			log.Printf("dedupe delivery %s: %v", delivery, err)
			c.String(http.StatusInternalServerError, "dedupe")
			return
		}
		if !isNew {
			c.String(http.StatusOK, "duplicate")
			return
		}
	}

	var env github.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		c.String(http.StatusBadRequest, "parse payload")
		return
	}

	if key, ok := DeriveRound(env); ok {
		if err := h.store.RecordRound(key.Owner, key.Repo, key.Number, key.SHA); err != nil {
			log.Printf("record round %s: %v", key, err)
			c.String(http.StatusInternalServerError, "record round")
			return
		}
		log.Printf("bucketed %q into round %s", event, key)
	}

	c.String(http.StatusAccepted, "accepted")
}
