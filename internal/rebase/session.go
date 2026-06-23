package rebase

import (
	"context"
	"fmt"
	"log"
	"time"
)

// LeaseRenewer is satisfied by *watcher.Client — extracted as an interface so
// that tests can inject a fake without depending on the watcher package.
type LeaseRenewer interface {
	RenewRebaseLease(ctx context.Context, owner, repo string, number int, holder string) error
	ReleaseRebaseLease(ctx context.Context, owner, repo string, number int, holder string) error
}

// noopRenewer is a LeaseRenewer that silently succeeds (used in tests).
type noopRenewer struct{}

func (noopRenewer) RenewRebaseLease(_ context.Context, _, _ string, _ int, _ string) error {
	return nil
}
func (noopRenewer) ReleaseRebaseLease(_ context.Context, _, _ string, _ int, _ string) error {
	return nil
}

// heartbeatInterval is the default cadence at which the session sends a
// heartbeat while rebasing. Must be comfortably shorter than the Hub lease TTL.
// It is the fallback when no WithHeartbeatInterval option is supplied, so the
// behavior is unchanged unless an operator tunes CAW_REBASE_HEARTBEAT.
const heartbeatInterval = 30 * time.Second

// Session performs the normal-path rebase for an agent-owned PR (ADR-0002).
// It uses the already-acquired lease identified by cfg.Holder, sends periodic
// heartbeats to the Hub, runs git fetch/rebase/force-push, then releases the lease.
type Session struct {
	runner    GitRunner
	renewer   LeaseRenewer
	heartbeat time.Duration
}

// SessionOption configures a Session.
type SessionOption func(*Session)

// WithHeartbeatInterval overrides the lease-heartbeat cadence. A non-positive
// value is ignored, leaving the default heartbeatInterval in place.
func WithHeartbeatInterval(d time.Duration) SessionOption {
	return func(s *Session) {
		if d > 0 {
			s.heartbeat = d
		}
	}
}

// NewSession constructs a Session with the given GitRunner and LeaseRenewer.
// The heartbeat cadence defaults to heartbeatInterval; override it with
// WithHeartbeatInterval.
func NewSession(runner GitRunner, renewer LeaseRenewer, opts ...SessionOption) *Session {
	if renewer == nil {
		renewer = noopRenewer{}
	}
	s := &Session{runner: runner, renewer: renewer, heartbeat: heartbeatInterval}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run performs the full rebase sequence:
//  1. Start a background heartbeat goroutine so the Hub does not expire the lease.
//  2. git fetch <remote>
//  3. git rebase <base>
//  4. git push --force-with-lease <remote> <branch>
//  5. Cancel the heartbeat, release the lease.
//
// Returns the first error encountered. The lease is released even on error
// (best-effort).
func (s *Session) Run(ctx context.Context, cfg Config) error {
	// heartbeat loop
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(s.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := s.renewer.RenewRebaseLease(hbCtx, cfg.Owner, cfg.Repo, cfg.Number, cfg.Holder); err != nil {
					log.Printf("rebase session %s/%s#%d: heartbeat: %v", cfg.Owner, cfg.Repo, cfg.Number, err)
				}
			}
		}
	}()

	var rebaseErr error
	if err := s.runner.Fetch(ctx, cfg.Remote); err != nil {
		rebaseErr = fmt.Errorf("fetch: %w", err)
	}
	if rebaseErr == nil {
		if err := s.runner.Rebase(ctx, cfg.Branch, cfg.Base); err != nil {
			rebaseErr = fmt.Errorf("rebase: %w", err)
		}
	}
	if rebaseErr == nil {
		if err := s.runner.ForcePushWithLease(ctx, cfg.Remote, cfg.Branch); err != nil {
			rebaseErr = fmt.Errorf("force-push: %w", err)
		}
	}

	// Stop heartbeat before releasing.
	hbCancel()

	// Release lease best-effort.
	if err := s.renewer.ReleaseRebaseLease(ctx, cfg.Owner, cfg.Repo, cfg.Number, cfg.Holder); err != nil {
		log.Printf("rebase session %s/%s#%d: release lease: %v", cfg.Owner, cfg.Repo, cfg.Number, err)
	}

	return rebaseErr
}
