package hub

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
)

// defaultLeaseDuration is the TTL granted per successful lease acquisition (ADR-0005).
// Slice 6 (#6) will add heartbeat renewal; for now the lease is fixed-duration.
const defaultLeaseDuration = int64(5 * 60) // 5 minutes in seconds

// HandleAcquireLease serves POST /leases/:owner/:repo/:number.
// The authenticated installation_id is extracted from context (set by auth.Required).
// On success (lease granted) 200 is returned with the lease JSON.
// On denial (another holder is active) 409 Conflict is returned.
// Rebase EXECUTION, heartbeat-during-rebase, and orphan fallback are Slice 6 — TODO(#6).
func (h *Hub) HandleAcquireLease(c *gin.Context) {
	owner := c.Param("owner")
	repo := c.Param("repo")
	numStr := c.Param("number")

	if owner == "" || repo == "" || numStr == "" {
		c.String(http.StatusBadRequest, "owner, repo, and number are required")
		return
	}

	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 {
		c.String(http.StatusBadRequest, "number must be a positive integer")
		return
	}

	// Caller identity comes from the verified installation token (ADR-0003).
	installationID, _ := c.Get(auth.ContextInstallationID)
	holder, ok := installationID.(string)
	if !ok || holder == "" {
		c.String(http.StatusUnauthorized, "installation id missing from context")
		return
	}

	res, err := h.store.AcquireLease(owner, repo, num, holder, defaultLeaseDuration)
	if err != nil {
		log.Printf("acquire lease %s/%s#%d: %v", owner, repo, num, err)
		c.String(http.StatusInternalServerError, "lease")
		return
	}

	// Defense-in-depth (ADR-0005): the store grants atomically against the lease
	// TTL, but if a denial comes back referencing a lease that has *already*
	// expired (clock skew or a stale read between the upsert and the read-back),
	// treat the expired holder as no-holder and retry the acquire once. An expired
	// lease must never block a fresh holder at the API boundary.
	if !res.Granted && leaseExpired(res.Lease.ExpiresAt) {
		res, err = h.store.AcquireLease(owner, repo, num, holder, defaultLeaseDuration)
		if err != nil {
			log.Printf("re-acquire expired lease %s/%s#%d: %v", owner, repo, num, err)
			c.String(http.StatusInternalServerError, "lease")
			return
		}
	}

	type leaseResponse struct {
		Granted         bool   `json:"granted"`
		Holder          string `json:"holder"`
		ExpiresAt       int64  `json:"expires_at"`
		LastHeartbeatAt int64  `json:"last_heartbeat_at"`
		AcquiredAt      int64  `json:"acquired_at"`
		// TODO(#6): heartbeat endpoint for lease renewal during rebase execution.
	}

	resp := leaseResponse{
		Granted:         res.Granted,
		Holder:          res.Lease.Holder,
		ExpiresAt:       res.Lease.ExpiresAt,
		LastHeartbeatAt: res.Lease.LastHeartbeatAt,
		AcquiredAt:      res.Lease.AcquiredAt,
	}

	if !res.Granted {
		// 409: lease is held by another installation; body tells the caller who holds it.
		c.JSON(http.StatusConflict, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// leaseExpired reports whether the lease has passed its expiry time (ADR-0005).
// An expired lease is treated as no-holder so a fresh holder may acquire it.
func leaseExpired(expiresAt int64) bool {
	return time.Now().Unix() >= expiresAt
}
