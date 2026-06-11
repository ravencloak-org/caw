// Package settle schedules Round settles: a grace window after a trigger,
// re-armed by late same-SHA signals (re-settle, ADR-0004). On fire it compiles
// the Round's signals into one summary and fans it out, or stores it as pending
// when no Session is listening (ADR-0006).
package settle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ravencloak-org/caw/internal/compile"
	"github.com/ravencloak-org/caw/internal/store"
)

// DefaultGrace is the settle grace window after the latest trigger.
const DefaultGrace = 30 * time.Second

// Publisher fans a serialized summary out to subscribers of a PR key.
type Publisher interface {
	Publish(key string, msg []byte) int
}

// MergeabilityPoller polls a PR's mergeability after a Round settles — the one
// poll in the system (CONTEXT.md). It returns a signal to fold into the summary;
// ok=false skips it.
type MergeabilityPoller interface {
	Mergeability(owner, repo string, number int, sha string) (store.Signal, bool, error)
}

// OrphanRebaseHandler is notified when a PR is orphaned (zero live sessions)
// and the mergeability poll shows it is behind its base branch. The Hub
// acquires the lease and performs the rebase/auto-merge on behalf of the PR.
type OrphanRebaseHandler interface {
	TriggerOrphanRebase(ctx context.Context, owner, repo string, number int, sha string) error
}

// Option configures an Engine.
type Option func(*Engine)

// WithPoller wires a mergeability poller invoked at each settle.
func WithPoller(p MergeabilityPoller) Option {
	return func(e *Engine) { e.poller = p }
}

// WithOrphanRebaseHandler wires an OrphanRebaseHandler that is invoked when
// a PR settle is orphaned and the PR is behind its base branch.
func WithOrphanRebaseHandler(h OrphanRebaseHandler) Option {
	return func(e *Engine) { e.orphanHandler = h }
}

// Engine schedules and fires Round settles.
type Engine struct {
	store         *store.Store
	pub           Publisher
	grace         time.Duration
	poller        MergeabilityPoller  // optional; nil disables the mergeability poll
	orphanHandler OrphanRebaseHandler // optional; nil disables orphan rebase fallback

	mu     sync.Mutex
	rounds map[string]*roundState
}

type roundState struct {
	owner, repo string
	number      int
	sha         string
	timer       *time.Timer
	seq         int
}

// New builds an Engine. A non-positive grace falls back to DefaultGrace.
func New(st *store.Store, pub Publisher, grace time.Duration, opts ...Option) *Engine {
	if grace <= 0 {
		grace = DefaultGrace
	}
	e := &Engine{store: st, pub: pub, grace: grace, rounds: make(map[string]*roundState)}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func roundKey(owner, repo string, number int, sha string) string {
	return fmt.Sprintf("%s/%s#%d@%s", owner, repo, number, sha)
}

// PRKey is the SSE subscription key for a PR (owner/repo#number), independent of SHA.
// isBehindBase reports whether any signal group in the summary contains a
// "behind base" item — the indicator that the PR needs a rebase.
func isBehindBase(groups []compile.SignalGroup) bool {
	for _, g := range groups {
		for _, item := range g.Items {
			if item.Body == "behind base" {
				return true
			}
		}
	}
	return false
}

// PRKey returns the canonical string key for a pull request: "owner/repo#number".
func PRKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// Touch (re)arms the grace timer for a Round. The first call opens the settle;
// a later same-SHA call re-settles it (ADR-0004).
func (e *Engine) Touch(owner, repo string, number int, sha string) {
	rk := roundKey(owner, repo, number, sha)
	e.mu.Lock()
	defer e.mu.Unlock()
	rs := e.rounds[rk]
	if rs == nil {
		rs = &roundState{owner: owner, repo: repo, number: number, sha: sha}
		e.rounds[rk] = rs
	}
	if rs.timer != nil {
		rs.timer.Stop()
	}
	rs.timer = time.AfterFunc(e.grace, func() { e.fire(rk) })
}

// FireNow settles a Round immediately, bypassing the grace timer.
func (e *Engine) FireNow(owner, repo string, number int, sha string) {
	rk := roundKey(owner, repo, number, sha)
	e.mu.Lock()
	rs := e.rounds[rk]
	if rs == nil {
		rs = &roundState{owner: owner, repo: repo, number: number, sha: sha}
		e.rounds[rk] = rs
	}
	if rs.timer != nil {
		rs.timer.Stop()
	}
	e.mu.Unlock()
	e.fire(rk)
}

// fire compiles and dispatches a settle for the given Round.
func (e *Engine) fire(rk string) {
	e.mu.Lock()
	rs := e.rounds[rk]
	if rs == nil {
		e.mu.Unlock()
		return
	}
	rs.seq++
	seq := rs.seq
	owner, repo, number, sha := rs.owner, rs.repo, rs.number, rs.sha
	e.mu.Unlock()

	tracer := otel.Tracer("caw-hub/settle")
	ctx, span := tracer.Start(context.Background(), "settle.fire")
	defer span.End()
	span.SetAttributes(
		attribute.String("settle.round_key", rk),
		attribute.Int("settle.seq", seq),
		attribute.String("github.owner", owner),
		attribute.String("github.repo", repo),
		attribute.Int("github.pr_number", number),
		attribute.String("github.sha", sha),
	)

	// The one poll in the system: re-verify mergeability at settle time and fold
	// it in as a signal (ADR-0004). A stable ExternalID replaces the prior poll.
	if e.poller != nil {
		switch sig, ok, err := e.poller.Mergeability(owner, repo, number, sha); {
		case err != nil:
			log.Printf("settle %s: mergeability poll: %v", rk, err)
		case ok:
			if err := e.store.AddSignal(sig); err != nil {
				log.Printf("settle %s: add mergeability: %v", rk, err)
			}
		}
	}

	signals, err := e.store.SignalsForRound(owner, repo, number, sha)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "load signals")
		log.Printf("settle %s: load signals: %v", rk, err)
		return
	}
	summary := compile.Compile(rk, seq, signals)
	msg, err := json.Marshal(summary)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal")
		log.Printf("settle %s: marshal: %v", rk, err)
		return
	}

	fanOut := e.pub.Publish(PRKey(owner, repo, number), msg)
	span.SetAttributes(attribute.Int("settle.fan_out", fanOut))
	if fanOut == 0 {
		// Orphaned: no live Session. Persist as pending, latest per signal-type.
		for _, g := range summary.Groups {
			if err := e.store.UpsertPending(store.PendingItem{
				Owner: owner, Repo: repo, Number: number,
				SignalType: g.Type, SHA: sha, PRState: "open", Summary: string(msg),
			}); err != nil {
				log.Printf("settle %s: pend %s: %v", rk, g.Type, err)
			}
		}

		// Orphan rebase fallback (ADR-0002 + ADR-0007): if the PR is behind
		// its base branch and there are no live subscribers, the Hub takes over
		// the rebase.
		if e.orphanHandler != nil && isBehindBase(summary.Groups) {
			if err := e.orphanHandler.TriggerOrphanRebase(ctx, owner, repo, number, sha); err != nil {
				log.Printf("settle %s: orphan rebase trigger: %v", rk, err)
			}
		}
	}
}
