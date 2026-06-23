package rebase

import (
	"testing"
	"time"
)

// TestWithHeartbeatInterval covers the SessionOption returned by
// WithHeartbeatInterval: a positive duration overrides the default cadence,
// while a zero or negative duration is ignored, leaving the default in place.
func TestWithHeartbeatInterval(t *testing.T) {
	runner := &fakeGitRunner{}

	t.Run("positive overrides default", func(t *testing.T) {
		s := NewSession(runner, noopRenewer{}, WithHeartbeatInterval(5*time.Second))
		if s.heartbeat != 5*time.Second {
			t.Errorf("heartbeat = %s, want 5s", s.heartbeat)
		}
	})

	t.Run("zero is ignored", func(t *testing.T) {
		s := NewSession(runner, noopRenewer{}, WithHeartbeatInterval(0))
		if s.heartbeat != heartbeatInterval {
			t.Errorf("heartbeat = %s, want default %s", s.heartbeat, heartbeatInterval)
		}
	})

	t.Run("negative is ignored", func(t *testing.T) {
		s := NewSession(runner, noopRenewer{}, WithHeartbeatInterval(-3*time.Second))
		if s.heartbeat != heartbeatInterval {
			t.Errorf("heartbeat = %s, want default %s", s.heartbeat, heartbeatInterval)
		}
	})
}

// TestWithLeaseTTL covers the OrphanHandlerOption returned by WithLeaseTTL:
// a positive value overrides the default store-level lease TTL, while a zero
// or negative value is ignored, leaving the default in place.
func TestWithLeaseTTL(t *testing.T) {
	st := &fakeLeaseStore{}
	runner := &fakeGitRunner{}
	merger := &fakeAutoMerger{}

	t.Run("positive overrides default", func(t *testing.T) {
		h := NewOrphanHandler("hub-orphan", st, runner, merger, WithLeaseTTL(120))
		if h.leaseTTL != 120 {
			t.Errorf("leaseTTL = %d, want 120", h.leaseTTL)
		}
	})

	t.Run("zero is ignored", func(t *testing.T) {
		h := NewOrphanHandler("hub-orphan", st, runner, merger, WithLeaseTTL(0))
		if h.leaseTTL != orphanLeaseTTL {
			t.Errorf("leaseTTL = %d, want default %d", h.leaseTTL, orphanLeaseTTL)
		}
	})

	t.Run("negative is ignored", func(t *testing.T) {
		h := NewOrphanHandler("hub-orphan", st, runner, merger, WithLeaseTTL(-10))
		if h.leaseTTL != orphanLeaseTTL {
			t.Errorf("leaseTTL = %d, want default %d", h.leaseTTL, orphanLeaseTTL)
		}
	})
}

// TestWithOrphanHeartbeatInterval covers the OrphanHandlerOption returned by
// WithOrphanHeartbeatInterval: a positive duration overrides the default
// renewal cadence, while a zero or negative duration is ignored.
func TestWithOrphanHeartbeatInterval(t *testing.T) {
	st := &fakeLeaseStore{}
	runner := &fakeGitRunner{}
	merger := &fakeAutoMerger{}

	t.Run("positive overrides default", func(t *testing.T) {
		h := NewOrphanHandler("hub-orphan", st, runner, merger, WithOrphanHeartbeatInterval(7*time.Second))
		if h.heartbeat != 7*time.Second {
			t.Errorf("heartbeat = %s, want 7s", h.heartbeat)
		}
	})

	t.Run("zero is ignored", func(t *testing.T) {
		h := NewOrphanHandler("hub-orphan", st, runner, merger, WithOrphanHeartbeatInterval(0))
		if h.heartbeat != heartbeatInterval {
			t.Errorf("heartbeat = %s, want default %s", h.heartbeat, heartbeatInterval)
		}
	})

	t.Run("negative is ignored", func(t *testing.T) {
		h := NewOrphanHandler("hub-orphan", st, runner, merger, WithOrphanHeartbeatInterval(-2*time.Second))
		if h.heartbeat != heartbeatInterval {
			t.Errorf("heartbeat = %s, want default %s", h.heartbeat, heartbeatInterval)
		}
	})
}
