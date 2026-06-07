package config

import "testing"

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
