package rebase

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ravencloak-org/caw/internal/store"
)

// AutoMerger enables GitHub's auto-merge feature for a PR after a successful
// rebase. Satisfied by *ghclient.Client.
type AutoMerger interface {
	EnableAutoMerge(ctx context.Context, owner, repo string, number int, mergeMethod string) error
}

// LeaseStore is the subset of *store.Store needed by the orphan handler.
// *store.Store satisfies this interface directly — no adapter required.
type LeaseStore interface {
	AcquireLease(org, repo string, prNumber int, holder string, leaseDuration int64) (store.AcquireLeaseResult, error)
	RenewLease(org, repo string, prNumber int, holder string, extendBy int64) (store.Lease, error)
	ReleaseLease(org, repo string, prNumber int, holder string) error
}

// orphanLeaseTTL is the store-level lease TTL in seconds used by the Hub orphan
// handler — mirrors hub.leaseTTL (90 s).
const orphanLeaseTTL = int64(90)

// OrphanHandler implements settle.OrphanRebaseHandler. When a PR is orphaned
// (no live SSE subscribers) and behind its base branch, it:
//  1. Acquires the Hub lease from the store.
//  2. git fetch / rebase / force-push using the provided GitRunner.
//  3. Calls EnableAutoMerge on the GitHub client.
//  4. Releases the lease.
type OrphanHandler struct {
	holderID string // e.g. the Hub's installation ID
	store    LeaseStore
	runner   GitRunner
	merger   AutoMerger
	branchFn func(owner, repo string, number int) Config // builds git config for the PR
}

// OrphanHandlerOption configures an OrphanHandler.
type OrphanHandlerOption func(*OrphanHandler)

// WithBranchResolver sets a function that resolves the git Config for a given PR.
// If not set, a default branch naming convention of "pr-{number}" is used.
func WithBranchResolver(fn func(owner, repo string, number int) Config) OrphanHandlerOption {
	return func(h *OrphanHandler) { h.branchFn = fn }
}

// NewOrphanHandler builds an OrphanHandler. holderID is the lease holder
// identity string (e.g. the Hub installation ID or a stable unique string).
func NewOrphanHandler(holderID string, st LeaseStore, runner GitRunner, merger AutoMerger, opts ...OrphanHandlerOption) *OrphanHandler {
	h := &OrphanHandler{
		holderID: holderID,
		store:    st,
		runner:   runner,
		merger:   merger,
	}
	for _, o := range opts {
		o(h)
	}
	if h.branchFn == nil {
		h.branchFn = defaultBranchConfig
	}
	return h
}

func defaultBranchConfig(owner, repo string, number int) Config {
	return Config{
		Owner:  owner,
		Repo:   repo,
		Number: number,
		Branch: fmt.Sprintf("pr-%d", number),
		Remote: "origin",
		Base:   "origin/main",
	}
}

// TriggerOrphanRebase implements settle.OrphanRebaseHandler. It acquires the
// Hub-granted lease from the store, performs the rebase with periodic heartbeats,
// enables GitHub auto-merge, and releases the lease.
func (h *OrphanHandler) TriggerOrphanRebase(ctx context.Context, owner, repo string, number int, sha string) error {
	res, err := h.store.AcquireLease(owner, repo, number, h.holderID, orphanLeaseTTL)
	if err != nil {
		return fmt.Errorf("orphan rebase %s/%s#%d: acquire lease: %w", owner, repo, number, err)
	}
	if !res.Granted {
		// Another holder owns the lease (session-side rebase in progress).
		log.Printf("orphan rebase %s/%s#%d: lease held by %q; skipping", owner, repo, number, res.Lease.Holder)
		return nil
	}

	cfg := h.branchFn(owner, repo, number)
	cfg.SHA = sha
	cfg.Holder = h.holderID

	// Heartbeat loop while rebasing.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if _, err := h.store.RenewLease(owner, repo, number, h.holderID, orphanLeaseTTL); err != nil {
					log.Printf("orphan rebase %s/%s#%d: heartbeat: %v", owner, repo, number, err)
				}
			}
		}
	}()

	var rebaseErr error
	if err := h.runner.Fetch(ctx, cfg.Remote); err != nil {
		rebaseErr = fmt.Errorf("fetch: %w", err)
	}
	if rebaseErr == nil {
		if err := h.runner.Rebase(ctx, cfg.Branch, cfg.Base); err != nil {
			rebaseErr = fmt.Errorf("rebase: %w", err)
		}
	}
	if rebaseErr == nil {
		if err := h.runner.ForcePushWithLease(ctx, cfg.Remote, cfg.Branch); err != nil {
			rebaseErr = fmt.Errorf("force-push: %w", err)
		}
	}

	// Stop heartbeat before any final operations.
	hbCancel()

	// Auto-merge after a clean rebase.
	if rebaseErr == nil && h.merger != nil {
		if err := h.merger.EnableAutoMerge(ctx, owner, repo, number, "merge"); err != nil {
			log.Printf("orphan rebase %s/%s#%d: enable auto-merge: %v", owner, repo, number, err)
			// Non-fatal: log only; the rebase itself succeeded.
		}
	}

	// Release lease best-effort.
	if err := h.store.ReleaseLease(owner, repo, number, h.holderID); err != nil {
		log.Printf("orphan rebase %s/%s#%d: release lease: %v", owner, repo, number, err)
	}

	return rebaseErr
}
