// Package repoaccess implements the Auth v2 per-user repo-access decision
// cache that gates /sse/..., /pending, and /leases/... on whether the GitHub
// user behind the presented Hub token actually has read access to the
// requested repository.
//
// A decision is one of: allow (positive), deny (negative), or unavailable
// (GitHub is unreachable AND we have no usable prior decision). The cache
// keeps positive decisions for 5 minutes and negative ones for 60 seconds,
// and within a 30-minute grace window on top of a positive decision will
// continue to serve allow when GitHub returns 5xx — protecting long-lived
// SSE streams from a transient outage knocking everyone offline.
//
// The cache key is (installation_id, github_user_id, owner, repo): including
// installation_id guarantees that an installation-level invalidation (the
// `installation.deleted` webhook) drops every cached decision tied to that
// installation, and including github_user_id guarantees that two different
// users can never see each other's cached allow.
//
// See docs/adr/0003 + the Auth v2 design doc §"Authorization model — per-user
// repo access" for the full rationale.
package repoaccess

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Errors returned by Lookup (wrapped). Middleware inspects them with errors.Is
// to map onto distinct HTTP responses.
var (
	// ErrUnavailable signals that GitHub could not be reached AND no usable
	// prior decision was available. Middleware responds 503 with Retry-After.
	ErrUnavailable = errors.New("repoaccess: github unavailable")
	// ErrConfigError signals an App-permissions / scope misconfiguration on
	// the Hub side (GitHub returned 403). Middleware responds 500 — this is
	// an operator bug, not a user-facing denial.
	ErrConfigError = errors.New("repoaccess: github config error")
)

// Source values returned alongside Lookup's decision, intended for span
// attributes / structured logging. Stable strings; tests assert on them.
const (
	SourceHit         = "hit"          // positive cache hit
	SourceNegativeHit = "negative-hit" // negative cache hit (cached deny within TTL)
	SourceMissAllow   = "miss-allow"   // miss, fetched, allowed
	SourceMissDeny    = "miss-deny"    // miss, fetched, denied (404 from GitHub)
	SourceStale       = "stale-allow"  // positive entry past TTL but within grace; GitHub down
	SourceUnavailable = "unavailable"  // GitHub down, no usable prior entry → fail-closed
	SourceConfigError = "config-error" // 403 from GitHub → operator misconfiguration
	SourceLegacy      = "legacy"       // userID == 0, middleware should have bypassed before calling
)

// Checker is the mockable seam Cache uses to ask GitHub whether userLogin has
// read access to owner/repo under installationID. The real implementation
// lives in github.go; tests inject a fake to control timing and outcomes.
type Checker interface {
	HasReadAccess(ctx context.Context, installationID, userLogin, owner, repo string) (allowed bool, err error)
}

// Defaults documented per the Auth v2 plan §"Authorization model".
const (
	DefaultPositiveTTL = 5 * time.Minute
	DefaultNegativeTTL = 60 * time.Second
	DefaultStaleGrace  = 30 * time.Minute
	DefaultSweepEvery  = 1 * time.Minute
)

// Options configures a Cache. Zero-valued fields fall back to the package
// defaults above; nil NowFn defaults to time.Now.
type Options struct {
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	StaleGrace  time.Duration
	SweepEvery  time.Duration
	NowFn       func() time.Time
}

type entry struct {
	allowed   bool
	fetchedAt time.Time
	expiresAt time.Time
}

// Cache is a process-local map of per-(installation, user, repo) access
// decisions. It is safe for concurrent use. The optional sweeper goroutine
// (Start) evicts expired entries; the cache stays correct without the
// sweeper — Lookup re-validates on every read — but unbounded growth across
// installation churn argues for the periodic sweep.
type Cache struct {
	mu       sync.Mutex
	entries  map[string]entry
	inflight map[string]chan struct{}

	checker    Checker
	nowFn      func() time.Time
	posTTL     time.Duration
	negTTL     time.Duration
	staleGrace time.Duration
	sweepEvery time.Duration
}

// NewCache constructs a Cache. The checker may be nil (tests that exercise
// only the bypass path or pre-seeded entries); Lookup returns ErrUnavailable
// when it is asked to fetch with a nil checker.
func NewCache(checker Checker, opts Options) *Cache {
	c := &Cache{
		entries:    make(map[string]entry),
		inflight:   make(map[string]chan struct{}),
		checker:    checker,
		nowFn:      opts.NowFn,
		posTTL:     opts.PositiveTTL,
		negTTL:     opts.NegativeTTL,
		staleGrace: opts.StaleGrace,
		sweepEvery: opts.SweepEvery,
	}
	if c.nowFn == nil {
		c.nowFn = time.Now
	}
	if c.posTTL == 0 {
		c.posTTL = DefaultPositiveTTL
	}
	if c.negTTL == 0 {
		c.negTTL = DefaultNegativeTTL
	}
	if c.staleGrace == 0 {
		c.staleGrace = DefaultStaleGrace
	}
	if c.sweepEvery == 0 {
		c.sweepEvery = DefaultSweepEvery
	}
	return c
}

// cacheKey serializes the lookup tuple to a single string. Layout
// "<instID>/<userID>/<owner>/<repo>" lets FlushInstallation and FlushRepo do
// cheap HasPrefix / HasSuffix scans without a parsed-key index.
func cacheKey(instID string, userID int64, owner, repo string) string {
	return fmt.Sprintf("%s/%d/%s/%s", instID, userID, owner, repo)
}

// Lookup resolves whether userLogin (with id userID) may read owner/repo
// under installationID. A userID of 0 is the legacy-token sentinel — callers
// (RequireRepoAccess) bypass before reaching here; Lookup mirrors that as a
// safety net rather than caching legacy lookups.
//
// Return contract:
//   - (true, SourceHit|SourceMissAllow|SourceStale, nil)         → allow
//   - (false, SourceNegativeHit|SourceMissDeny, nil)             → deny
//   - (false, SourceUnavailable, ErrUnavailable-wrapped err)     → 5xx, fail-closed
//   - (false, SourceConfigError, ErrConfigError-wrapped err)     → 403, operator bug
func (c *Cache) Lookup(ctx context.Context, installationID string, userID int64, userLogin, owner, repo string) (bool, string, error) {
	if userID == 0 {
		// Defensive: middleware should already have bypassed legacy tokens.
		return false, SourceLegacy, nil
	}
	if installationID == "" || owner == "" || repo == "" || userLogin == "" {
		// Malformed lookup — refuse to cache or fetch. Middleware should
		// never construct one of these, but failing-closed beats logging.
		return false, SourceUnavailable, fmt.Errorf("%w: missing key components", ErrUnavailable)
	}

	key := cacheKey(installationID, userID, owner, repo)
	now := c.nowFn()

	// Coordinate concurrent cold-miss lookups for the same key through a
	// per-key inflight channel: only the first goroutine fetches, the rest
	// wait and re-read the cache. Plain map + sync.Mutex avoids pulling in
	// golang.org/x/sync just for singleflight.
	for {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok && now.Before(e.expiresAt) {
			c.mu.Unlock()
			if e.allowed {
				return true, SourceHit, nil
			}
			return false, SourceNegativeHit, nil
		}
		if ch, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			select {
			case <-ch:
				continue // re-read after the in-flight fetch completes
			case <-ctx.Done():
				return false, SourceUnavailable, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
			}
		}
		ch := make(chan struct{})
		c.inflight[key] = ch
		// Snapshot the prior entry (if any) for the stale-allow check below.
		prior, hasPrior := c.entries[key]
		c.mu.Unlock()

		allowed, source, err := c.fetchAndStore(ctx, key, now, prior, hasPrior, installationID, userLogin, owner, repo)

		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		close(ch)

		return allowed, source, err
	}
}

// fetchAndStore invokes the checker, applies stale-allow on transient
// failure, and writes the cache on success. Called exactly once per
// inflight-key slot; isolated from Lookup's loop so the inflight cleanup is
// deterministic regardless of return path.
func (c *Cache) fetchAndStore(ctx context.Context, key string, now time.Time, prior entry, hasPrior bool, installationID, userLogin, owner, repo string) (bool, string, error) {
	if c.checker == nil {
		return false, SourceUnavailable, fmt.Errorf("%w: no checker configured", ErrUnavailable)
	}

	allowed, err := c.checker.HasReadAccess(ctx, installationID, userLogin, owner, repo)
	if err != nil {
		if errors.Is(err, ErrConfigError) {
			return false, SourceConfigError, err
		}
		// Treat anything else as Unavailable. Apply stale-allow only on a
		// prior POSITIVE entry within the grace window — a stale negative
		// must NOT be widened to allow.
		if hasPrior && prior.allowed && now.Sub(prior.fetchedAt) < c.staleGrace {
			return true, SourceStale, nil
		}
		if !errors.Is(err, ErrUnavailable) {
			err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return false, SourceUnavailable, err
	}

	ttl := c.negTTL
	if allowed {
		ttl = c.posTTL
	}
	c.mu.Lock()
	c.entries[key] = entry{allowed: allowed, fetchedAt: now, expiresAt: now.Add(ttl)}
	c.mu.Unlock()

	if allowed {
		return true, SourceMissAllow, nil
	}
	return false, SourceMissDeny, nil
}

// FlushInstallation drops every entry for installationID. Called from the
// `installation.deleted` webhook handler — the entire installation is gone,
// every cached allow under it must die immediately.
func (c *Cache) FlushInstallation(installationID string) {
	prefix := installationID + "/"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// FlushRepo drops every entry for (installationID, fullName) regardless of
// user. Called from `installation_repositories.removed` — once the repo is
// no longer in the installation, no token under that installation should be
// able to tail it, even if a positive cache entry still says otherwise.
func (c *Cache) FlushRepo(installationID, fullName string) {
	prefix := installationID + "/"
	suffix := "/" + fullName // matches "<instID>/<userID>/<owner>/<repo>"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, suffix) {
			delete(c.entries, k)
		}
	}
}

// FlushUser drops every cache entry for userID across every installation and
// every repo. Called from Phase 4's POST /me/recover (panic button) so a
// stolen-and-revoked user has no in-memory allow surviving the persistence
// revoke. userID == 0 is a defensive no-op (the legacy-token sentinel never
// reaches the cache).
func (c *Cache) FlushUser(userID int64) {
	if userID == 0 {
		return
	}
	// Key layout: "<instID>/<userID>/<owner>/<repo>". The userID segment
	// is delimited by '/' on both sides; we scan with a /uid/ middle match
	// to avoid mistaking a prefix-of (e.g. user 4 matching keys for user 42).
	mid := fmt.Sprintf("/%d/", userID)
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.Contains(k, mid) {
			delete(c.entries, k)
		}
	}
}

// Len reports the current cached entry count. Useful for tests and metrics.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Start launches the background sweeper that evicts expired entries every
// sweepEvery interval. Returns immediately. The sweeper exits when ctx is
// canceled. Cache stays correct without the sweeper — Lookup re-validates
// on every read — Start exists only to bound memory across long-running
// processes with churn.
func (c *Cache) Start(ctx context.Context) {
	go c.runSweeper(ctx)
}

func (c *Cache) runSweeper(ctx context.Context) {
	t := time.NewTicker(c.sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweepOnce(c.nowFn())
		}
	}
}

// sweepOnce evicts expired entries past their stale-allow grace (positive)
// or past their TTL (negative). Exposed package-private for tests.
func (c *Cache) sweepOnce(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.allowed {
			if now.Sub(e.fetchedAt) >= c.staleGrace {
				delete(c.entries, k)
			}
		} else if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}
