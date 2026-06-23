package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{"CAW_ADDR", "CAW_DB", "CAW_GH_WEBHOOK_SECRET", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", c.Addr)
	}
	if c.DatabasePath != "caw.db" {
		t.Errorf("DatabasePath = %q, want caw.db", c.DatabasePath)
	}
	if c.GitHubWebhookSecret != "" {
		t.Errorf("GitHubWebhookSecret = %q, want empty", c.GitHubWebhookSecret)
	}
	if c.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", c.OTLPEndpoint)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("CAW_ADDR", ":9999")
	t.Setenv("CAW_DB", "/tmp/caw-test.db")
	t.Setenv("CAW_GH_WEBHOOK_SECRET", "shh")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://openobserve:5081")

	c := Load()
	if c.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", c.Addr)
	}
	if c.DatabasePath != "/tmp/caw-test.db" {
		t.Errorf("DatabasePath = %q", c.DatabasePath)
	}
	if c.GitHubWebhookSecret != "shh" {
		t.Errorf("GitHubWebhookSecret = %q, want shh", c.GitHubWebhookSecret)
	}
	if c.OTLPEndpoint != "http://openobserve:5081" {
		t.Errorf("OTLPEndpoint = %q", c.OTLPEndpoint)
	}
}

func TestLoadTuningKnobDefaults(t *testing.T) {
	for _, k := range []string{"CAW_SETTLE_GRACE", "CAW_REBASE_LEASE_TTL", "CAW_REBASE_HEARTBEAT"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.SettleGrace != 30*time.Second {
		t.Errorf("SettleGrace = %s, want 30s", c.SettleGrace)
	}
	if c.RebaseLeaseTTLSeconds != 90 {
		t.Errorf("RebaseLeaseTTLSeconds = %d, want 90", c.RebaseLeaseTTLSeconds)
	}
	if c.RebaseHeartbeat != 30*time.Second {
		t.Errorf("RebaseHeartbeat = %s, want 30s", c.RebaseHeartbeat)
	}
}

func TestLoadTuningKnobOverrides(t *testing.T) {
	t.Setenv("CAW_SETTLE_GRACE", "45s")
	t.Setenv("CAW_REBASE_LEASE_TTL", "120")
	t.Setenv("CAW_REBASE_HEARTBEAT", "15s")
	c := Load()
	if c.SettleGrace != 45*time.Second {
		t.Errorf("SettleGrace = %s, want 45s", c.SettleGrace)
	}
	if c.RebaseLeaseTTLSeconds != 120 {
		t.Errorf("RebaseLeaseTTLSeconds = %d, want 120", c.RebaseLeaseTTLSeconds)
	}
	if c.RebaseHeartbeat != 15*time.Second {
		t.Errorf("RebaseHeartbeat = %s, want 15s", c.RebaseHeartbeat)
	}
}

func TestLoadTuningKnobBadValuesFallBackToDefaults(t *testing.T) {
	// Unparseable and non-positive values must fall back to the defaults
	// rather than crash the Hub or apply a degenerate value.
	cases := []struct {
		name                  string
		grace, ttl, heartbeat string
	}{
		{"garbage", "not-a-duration", "abc", "fifteen"},
		{"zero", "0s", "0", "0s"},
		{"negative", "-5s", "-1", "-10s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CAW_SETTLE_GRACE", tc.grace)
			t.Setenv("CAW_REBASE_LEASE_TTL", tc.ttl)
			t.Setenv("CAW_REBASE_HEARTBEAT", tc.heartbeat)
			c := Load()
			if c.SettleGrace != 30*time.Second {
				t.Errorf("SettleGrace = %s, want default 30s", c.SettleGrace)
			}
			if c.RebaseLeaseTTLSeconds != 90 {
				t.Errorf("RebaseLeaseTTLSeconds = %d, want default 90", c.RebaseLeaseTTLSeconds)
			}
			if c.RebaseHeartbeat != 30*time.Second {
				t.Errorf("RebaseHeartbeat = %s, want default 30s", c.RebaseHeartbeat)
			}
		})
	}
}
