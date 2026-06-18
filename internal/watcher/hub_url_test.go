package watcher

import "testing"

func TestClient_HubURL(t *testing.T) {
	c := NewClient("https://hub.example.com", "tok-1")
	if got := c.HubURL(); got != "https://hub.example.com" {
		t.Errorf("HubURL = %q, want https://hub.example.com", got)
	}
	// Empty hub URL is allowed at construction (Auth v2: login tool can fall
	// back to env / argument). The getter just round-trips whatever was set.
	if got := NewClient("", "tok").HubURL(); got != "" {
		t.Errorf("empty HubURL should round-trip; got %q", got)
	}
}
