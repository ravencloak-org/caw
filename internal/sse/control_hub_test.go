package sse

import "testing"

func TestControlHubFanOut(t *testing.T) {
	h := NewControlHub()
	a := h.Subscribe(42)
	b := h.Subscribe(42)
	other := h.Subscribe(99)

	if got := h.CountSubscribers(42); got != 2 {
		t.Fatalf("CountSubscribers(42) = %d, want 2", got)
	}
	if !h.HasSubscribers(42) {
		t.Fatal("HasSubscribers(42) should be true")
	}

	evt := ControlEvent{Name: "pr_opened", Data: []byte(`{"owner":"o","repo":"r","number":1}`)}
	if n := h.Publish(42, evt); n != 2 {
		t.Fatalf("Publish delivered to %d, want 2", n)
	}
	for _, s := range []*ControlSubscriber{a, b} {
		got := <-s.C
		if got.Name != "pr_opened" || string(got.Data) != string(evt.Data) {
			t.Fatalf("subscriber got %+v, want %+v", got, evt)
		}
	}
	// Other user's subscriber must not receive anything.
	select {
	case msg := <-other.C:
		t.Fatalf("subscriber for user 99 received %+v", msg)
	default:
	}
}

func TestControlHubUnsubscribe(t *testing.T) {
	h := NewControlHub()
	s := h.Subscribe(7)
	h.Unsubscribe(s)

	if h.HasSubscribers(7) {
		t.Fatal("HasSubscribers should be false after unsubscribe")
	}
	if n := h.Publish(7, ControlEvent{Name: "ping"}); n != 0 {
		t.Fatalf("Publish after unsubscribe delivered %d, want 0", n)
	}
	if _, ok := <-s.C; ok {
		t.Fatal("subscriber channel should be closed after unsubscribe")
	}
	// Second Unsubscribe must be a safe no-op.
	h.Unsubscribe(s)
}

func TestControlHubPublishNoSubscribers(t *testing.T) {
	h := NewControlHub()
	if n := h.Publish(1234, ControlEvent{Name: "ping"}); n != 0 {
		t.Fatalf("Publish with no subscribers delivered %d, want 0", n)
	}
}

func TestControlHubPublishSkipsFullBuffer(t *testing.T) {
	h := NewControlHub()
	s := h.Subscribe(1)
	// Fill the buffer without draining.
	for i := range subBuffer {
		if n := h.Publish(1, ControlEvent{Name: "ping"}); n != 1 {
			t.Fatalf("buffered publish %d delivered %d, want 1", i, n)
		}
	}
	// Next publish must be dropped (non-blocking), not block the publisher.
	if n := h.Publish(1, ControlEvent{Name: "overflow"}); n != 0 {
		t.Fatalf("overflow publish delivered %d, want 0 (dropped)", n)
	}
	_ = s
}

// TestControlHubDoesNotLeakAcrossUsers proves keying on github_user_id keeps
// two users' events on independent channels — the cross-user isolation
// invariant the auth-v2 plan calls out explicitly.
func TestControlHubDoesNotLeakAcrossUsers(t *testing.T) {
	h := NewControlHub()
	alice := h.Subscribe(1)
	bob := h.Subscribe(2)

	h.Publish(1, ControlEvent{Name: "pr_opened", Data: []byte(`{"who":"alice"}`)})
	h.Publish(2, ControlEvent{Name: "pr_opened", Data: []byte(`{"who":"bob"}`)})

	got := <-alice.C
	if string(got.Data) != `{"who":"alice"}` {
		t.Fatalf("alice got %q, want alice payload", got.Data)
	}
	got = <-bob.C
	if string(got.Data) != `{"who":"bob"}` {
		t.Fatalf("bob got %q, want bob payload", got.Data)
	}
	select {
	case msg := <-alice.C:
		t.Fatalf("alice received bob's event: %+v", msg)
	default:
	}
}
