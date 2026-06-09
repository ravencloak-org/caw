// Package sse implements the Hub's fan-out of compiled summaries to Watchers
// over Server-Sent Events. Many Sessions may subscribe to one PR key; every
// listener receives every summary (ADR-0007).
package sse

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// noopCtx is a shared background context for metric recording calls inside
// Publish, which has no caller-supplied context.
var noopCtx = context.Background()

// fanOutCounter is the OTel metric used to record Publish fan-out sizes.
// It is initialized lazily on first use; if the MeterProvider is the global
// no-op (test / dev / no endpoint configured) the counter is a no-op too.
var (
	fanOutCounter     metric.Int64Counter
	fanOutCounterOnce sync.Once
)

func getFanOutCounter() metric.Int64Counter {
	fanOutCounterOnce.Do(func() {
		var err error
		fanOutCounter, err = otel.Meter("caw-hub/sse").Int64Counter(
			"sse.fan_out",
			metric.WithDescription("Number of subscribers that received a published message"),
		)
		if err != nil {
			// Fallback to a no-op counter so Publish never panics.
			fanOutCounter, _ = noop.NewMeterProvider().Meter("").Int64Counter("sse.fan_out")
		}
	})
	return fanOutCounter
}

// subBuffer is the per-subscriber channel depth. Summaries are infrequent, so a
// small buffer absorbs brief reader stalls without blocking the publisher.
const subBuffer = 16

// Subscriber is one live SSE connection bound to a PR key.
type Subscriber struct {
	C   chan []byte
	key string
}

// Hub fans messages out to all subscribers of a key (owner/repo#number).
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[*Subscriber]struct{}
}

// New returns an empty Hub.
func New() *Hub {
	return &Hub{subs: make(map[string]map[*Subscriber]struct{})}
}

// Subscribe registers and returns a Subscriber for key.
func (h *Hub) Subscribe(key string) *Subscriber {
	s := &Subscriber{C: make(chan []byte, subBuffer), key: key}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[key] == nil {
		h.subs[key] = make(map[*Subscriber]struct{})
	}
	h.subs[key][s] = struct{}{}
	return s
}

// Unsubscribe removes s and closes its channel. Safe to call once per Subscriber.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[s.key]
	if m == nil {
		return
	}
	if _, present := m[s]; !present {
		return
	}
	delete(m, s)
	if len(m) == 0 {
		delete(h.subs, s.key)
	}
	close(s.C)
}

// Publish delivers msg to every subscriber of key and returns how many received
// it. A subscriber whose buffer is full is skipped (non-blocking) so one slow
// reader cannot stall the publisher or other listeners.
func (h *Hub) Publish(key string, msg []byte) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	delivered := 0
	for s := range h.subs[key] {
		select {
		case s.C <- msg:
			delivered++
		default:
		}
	}
	getFanOutCounter().Add(noopCtx, int64(delivered), metric.WithAttributes(attribute.String("sse.key", key)))
	return delivered
}

// HasSubscribers reports whether key has at least one live subscriber.
// Zero subscribers means the PR is orphaned (ADR-0006/0007).
func (h *Hub) HasSubscribers(key string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[key]) > 0
}

// CountSubscribers returns the number of live subscribers for key.
func (h *Hub) CountSubscribers(key string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[key])
}
