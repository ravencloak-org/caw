package sse

import "testing"

func TestPublishFanOut(t *testing.T) {
	h := New()
	a := h.Subscribe("o/r#1")
	b := h.Subscribe("o/r#1")
	other := h.Subscribe("o/r#2")

	if got := h.CountSubscribers("o/r#1"); got != 2 {
		t.Fatalf("CountSubscribers = %d, want 2", got)
	}
	if !h.HasSubscribers("o/r#1") {
		t.Fatal("HasSubscribers should be true")
	}

	n := h.Publish("o/r#1", []byte("hello"))
	if n != 2 {
		t.Fatalf("Publish delivered to %d, want 2", n)
	}
	for _, s := range []*Subscriber{a, b} {
		if got := <-s.C; string(got) != "hello" {
			t.Fatalf("subscriber got %q, want hello", got)
		}
	}
	select {
	case msg := <-other.C:
		t.Fatalf("subscriber on other key received %q", msg)
	default:
	}
}

func TestUnsubscribe(t *testing.T) {
	h := New()
	s := h.Subscribe("k")
	h.Unsubscribe(s)

	if h.HasSubscribers("k") {
		t.Fatal("HasSubscribers should be false after unsubscribe")
	}
	if n := h.Publish("k", []byte("x")); n != 0 {
		t.Fatalf("Publish after unsubscribe delivered %d, want 0", n)
	}
	if _, ok := <-s.C; ok {
		t.Fatal("subscriber channel should be closed after unsubscribe")
	}
	// Second Unsubscribe must be a safe no-op.
	h.Unsubscribe(s)
}

func TestPublishNoSubscribers(t *testing.T) {
	h := New()
	if n := h.Publish("nobody", []byte("x")); n != 0 {
		t.Fatalf("Publish with no subscribers delivered %d, want 0", n)
	}
}

func TestPublishSkipsFullBuffer(t *testing.T) {
	h := New()
	s := h.Subscribe("k")
	// Fill the buffer without draining.
	for i := range subBuffer {
		if n := h.Publish("k", []byte("x")); n != 1 {
			t.Fatalf("buffered publish %d delivered %d, want 1", i, n)
		}
	}
	// Next publish must be dropped (non-blocking), not block the publisher.
	if n := h.Publish("k", []byte("overflow")); n != 0 {
		t.Fatalf("overflow publish delivered %d, want 0 (dropped)", n)
	}
	_ = s
}
