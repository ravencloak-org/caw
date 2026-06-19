// Package sse — control-plane fan-out for auth-v2 Phase 3.5 (issue #60).
//
// ControlHub is the SSE topology keyed on github_user_id, distinct from the
// per-PR Hub keyed on owner/repo#number. It carries notifications about the
// user's own activity — `pr_opened`, `pr_closed`, `installation_added` — so
// the MCP plugin can auto-subscribe to a PR the user just raised without the
// agent calling `subscribe_pr` first.
//
// Wire format matches the per-PR Hub: each event is a frame with `event:` and
// `data:` lines (see internal/sse/control_handler.go). Buffer sizing reuses
// subBuffer (16): control events are infrequent so no separate sizing.
package sse

import (
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// controlFanOutCounter is the OTel metric for per-user control fan-out sizes.
// Mirrors the per-PR Hub's lazy-init pattern.
var (
	controlFanOutCounter     metric.Int64Counter
	controlFanOutCounterOnce sync.Once
)

func getControlFanOutCounter() metric.Int64Counter {
	controlFanOutCounterOnce.Do(func() {
		var err error
		controlFanOutCounter, err = otel.Meter("caw-hub/sse").Int64Counter(
			"sse.control.fan_out",
			metric.WithDescription("Number of control subscribers that received a published event"),
		)
		if err != nil {
			controlFanOutCounter, _ = noop.NewMeterProvider().Meter("").Int64Counter("sse.control.fan_out")
		}
	})
	return controlFanOutCounter
}

// ControlEvent is one frame published to a user's control stream. Name is the
// SSE `event:` field (`pr_opened`, `pr_closed`, `installation_added`, `ping`);
// Data is the raw JSON body — the publisher pre-encodes so subscribers don't
// re-serialize on every fan-out.
type ControlEvent struct {
	Name string
	Data []byte
}

// ControlSubscriber is one live SSE connection bound to a github_user_id.
type ControlSubscriber struct {
	C      chan ControlEvent
	userID int64
}

// ControlHub fans control events out to all subscribers of a github_user_id.
type ControlHub struct {
	mu   sync.RWMutex
	subs map[int64]map[*ControlSubscriber]struct{}
}

// NewControlHub returns an empty ControlHub.
func NewControlHub() *ControlHub {
	return &ControlHub{subs: make(map[int64]map[*ControlSubscriber]struct{})}
}

// Subscribe registers and returns a ControlSubscriber for userID. The caller
// MUST eventually Unsubscribe to free the slot.
func (h *ControlHub) Subscribe(userID int64) *ControlSubscriber {
	s := &ControlSubscriber{C: make(chan ControlEvent, subBuffer), userID: userID}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[userID] == nil {
		h.subs[userID] = make(map[*ControlSubscriber]struct{})
	}
	h.subs[userID][s] = struct{}{}
	return s
}

// Unsubscribe removes s and closes its channel. Safe to call twice; the second
// call is a no-op (matches the per-PR Hub's Unsubscribe contract).
func (h *ControlHub) Unsubscribe(s *ControlSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[s.userID]
	if m == nil {
		return
	}
	if _, present := m[s]; !present {
		return
	}
	delete(m, s)
	if len(m) == 0 {
		delete(h.subs, s.userID)
	}
	close(s.C)
}

// Publish delivers evt to every subscriber of userID and returns how many
// received it. A subscriber whose buffer is full is skipped (non-blocking) so
// one slow reader cannot stall the publisher or other listeners — matches the
// per-PR Hub's slow-consumer policy.
func (h *ControlHub) Publish(userID int64, evt ControlEvent) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	delivered := 0
	for s := range h.subs[userID] {
		select {
		case s.C <- evt:
			delivered++
		default:
		}
	}
	getControlFanOutCounter().Add(noopCtx, int64(delivered),
		metric.WithAttributes(attribute.Int64("sse.control.user_id", userID)))
	return delivered
}

// HasSubscribers reports whether userID has at least one live control
// subscriber. Used by the webhook fan-out path to skip the per-user fetch on
// users with no live MCP connection.
func (h *ControlHub) HasSubscribers(userID int64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[userID]) > 0
}

// CountSubscribers returns the number of live control subscribers for userID.
func (h *ControlHub) CountSubscribers(userID int64) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[userID])
}
