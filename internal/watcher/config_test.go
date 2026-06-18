package watcher

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFromEnv_RequiresHubURL(t *testing.T) {
	t.Setenv(EnvHubURL, "")
	t.Setenv(EnvToken, "anything")
	_, err := ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), EnvHubURL) {
		t.Errorf("want %s-required error, got %v", EnvHubURL, err)
	}
}

func TestConfigFromEnv_TokenOptional(t *testing.T) {
	t.Setenv(EnvHubURL, "https://hub.test")
	t.Setenv(EnvToken, "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.HubURL != "https://hub.test" {
		t.Errorf("HubURL = %q, want https://hub.test", cfg.HubURL)
	}
	if cfg.Token != "" {
		t.Errorf("Token = %q, want empty (Auth v2: token optional)", cfg.Token)
	}
}

func TestConfigFromEnv_FullySet(t *testing.T) {
	t.Setenv(EnvHubURL, "https://hub.test")
	t.Setenv(EnvToken, "tok-123")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.HubURL != "https://hub.test" || cfg.Token != "tok-123" {
		t.Errorf("config mismatch: %+v", cfg)
	}
}

func TestDefaultCredentialsPath_EnvOverride(t *testing.T) {
	t.Setenv("CAW_CREDENTIALS_PATH", "/custom/path/creds.json")
	got, err := DefaultCredentialsPath()
	if err != nil {
		t.Fatalf("DefaultCredentialsPath: %v", err)
	}
	if got != "/custom/path/creds.json" {
		t.Errorf("got %q, want /custom/path/creds.json", got)
	}
}

func TestDefaultCredentialsPath_FallsBackToUserConfigDir(t *testing.T) {
	t.Setenv("CAW_CREDENTIALS_PATH", "")
	got, err := DefaultCredentialsPath()
	if err != nil {
		t.Fatalf("DefaultCredentialsPath: %v", err)
	}
	// We don't pin the exact prefix (varies per OS — XDG on Linux,
	// Library/Application Support on macOS, AppData on Windows) but
	// the path MUST end in caw/credentials.json.
	want := filepath.Join("caw", "credentials.json")
	if !strings.HasSuffix(got, want) {
		t.Errorf("got %q, want suffix %q", got, want)
	}
}
