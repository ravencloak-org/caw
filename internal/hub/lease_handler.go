package hub

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
)

// leaseTTL is the default TTL (in seconds) granted per successful lease
// acquisition or heartbeat renewal (ADR-0005). 90 s is chosen to be slightly
// longer than a typical git rebase + force-push round-trip while still expiring
// quickly enough that a crashed worker unblocks the next holder within two
// minutes. It is the fallback when the Hub is built without WithLeaseTTL, so the
// behavior is unchanged unless an operator tunes CAW_REBASE_LEASE_TTL; the
// per-Hub runtime value lives in Hub.leaseTTL.
const leaseTTL = int64(90)

// HandleAcquireLease serves POST /leases/:owner/:repo/:number.
// The authenticated installation_id is extracted from context (set by auth.Required).
// On success (lease granted) 200 is returned with the lease JSON.
// On denial (another holder is active) 409 Conflict is returned.
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
	// Defense-in-depth: the "setup" sentinel token must never acquire leases.
	// RequireRepoScope already blocks it, but guard here too for belt-and-suspenders.
	if holder == "setup" {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	res, err := h.store.AcquireLease(owner, repo, num, holder, h.leaseTTL)
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
		res, err = h.store.AcquireLease(owner, repo, num, holder, h.leaseTTL)
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

// HandleRenewLease serves PUT /leases/:owner/:repo/:number/heartbeat.
// The authenticated holder sends a heartbeat to extend expires_at by leaseTTL
// from now and update last_heartbeat_at. Only the current holder may renew.
// Returns 200 with updated lease JSON on success, 409 if not the holder or
// lease is expired, 500 on internal error.
func (h *Hub) HandleRenewLease(c *gin.Context) {
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

	installationID, _ := c.Get(auth.ContextInstallationID)
	holder, ok := installationID.(string)
	if !ok || holder == "" {
		c.String(http.StatusUnauthorized, "installation id missing from context")
		return
	}

	updated, err := h.store.RenewLease(owner, repo, num, holder, h.leaseTTL)
	if err != nil {
		log.Printf("renew lease %s/%s#%d holder=%s: %v", owner, repo, num, holder, err)
		c.JSON(http.StatusConflict, gin.H{"error": "not holder or lease expired"})
		return
	}

	type leaseResponse struct {
		Granted         bool   `json:"granted"`
		Holder          string `json:"holder"`
		ExpiresAt       int64  `json:"expires_at"`
		LastHeartbeatAt int64  `json:"last_heartbeat_at"`
		AcquiredAt      int64  `json:"acquired_at"`
	}

	c.JSON(http.StatusOK, leaseResponse{
		Granted:         true,
		Holder:          updated.Holder,
		ExpiresAt:       updated.ExpiresAt,
		LastHeartbeatAt: updated.LastHeartbeatAt,
		AcquiredAt:      updated.AcquiredAt,
	})
}

// HandleReleaseLease serves DELETE /leases/:owner/:repo/:number.
// The authenticated holder explicitly releases the lease so the next rebase
// waiter can acquire it immediately rather than waiting for TTL expiry.
// Returns 204 No Content on success, 409 if not the holder, 500 on error.
func (h *Hub) HandleReleaseLease(c *gin.Context) {
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

	installationID, _ := c.Get(auth.ContextInstallationID)
	holder, ok := installationID.(string)
	if !ok || holder == "" {
		c.String(http.StatusUnauthorized, "installation id missing from context")
		return
	}

	if err := h.store.ReleaseLease(owner, repo, num, holder); err != nil {
		log.Printf("release lease %s/%s#%d holder=%s: %v", owner, repo, num, holder, err)
		c.JSON(http.StatusConflict, gin.H{"error": "not holder"})
		return
	}

	c.Status(http.StatusNoContent)
}
