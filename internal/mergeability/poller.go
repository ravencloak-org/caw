// Package mergeability turns a GitHub mergeability poll into a Hub signal.
// It is the one poll in the system, run after a Round settles (CONTEXT.md).
package mergeability

import (
	"context"
	"time"

	"github.com/ravencloak-org/caw/internal/ghclient"
	"github.com/ravencloak-org/caw/internal/store"
)

// Poller adapts a GitHub client to settle.MergeabilityPoller.
type Poller struct {
	client  *ghclient.Client
	timeout time.Duration
}

// New returns a Poller backed by c.
func New(c *ghclient.Client) *Poller {
	return &Poller{client: c, timeout: 15 * time.Second}
}

// Mergeability polls the PR and returns the mergeability signal for the Round.
// A stable ExternalID means a re-poll replaces the prior signal (ADR-0006).
func (p *Poller) Mergeability(owner, repo string, number int, sha string) (store.Signal, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	ps, err := p.client.PullMergeability(ctx, owner, repo, number)
	if err != nil {
		return store.Signal{}, false, err
	}
	severity, body := classify(ps)
	return store.Signal{
		Owner: owner, Repo: repo, Number: number, SHA: sha,
		SignalType: "mergeability", Source: "poll", ExternalID: "mergeability",
		Severity: severity, Body: body,
	}, true, nil
}

// classify maps GitHub's mergeable_state to a severity + human body.
func classify(ps ghclient.PullState) (severity, body string) {
	switch ps.MergeableState {
	case "clean":
		return "", "clean"
	case "behind":
		return "MINOR", "behind base"
	case "dirty":
		return "MAJOR", "conflicts"
	case "blocked":
		return "MINOR", "blocked"
	case "unstable":
		return "MINOR", "unstable (failing required checks)"
	case "", "unknown":
		return "", "unknown"
	default:
		return "", ps.MergeableState
	}
}
