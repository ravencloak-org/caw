// Package config loads Hub configuration from the environment.
package config

import "os"

// Config holds the Hub's runtime configuration.
type Config struct {
	// Addr is the listen address for the HTTP server.
	Addr string
	// DatabasePath is the path to the embedded SQLite database.
	DatabasePath string
	// GitHubWebhookSecret verifies the X-Hub-Signature-256 header.
	GitHubWebhookSecret string
	// OTLPEndpoint, when set, is where telemetry is exported (ADR-0008).
	// Empty disables export.
	OTLPEndpoint string
	// GitHubToken authenticates the mergeability poll (stopgap until the
	// GitHub App installation tokens of Slice 5). Empty disables polling.
	GitHubToken string
	// GitHubAPIBase overrides the GitHub API base URL (tests / GH Enterprise).
	GitHubAPIBase string
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Addr:                getenv("CAW_ADDR", ":8080"),
		DatabasePath:        getenv("CAW_DB", "caw.db"),
		GitHubWebhookSecret: os.Getenv("CAW_GH_WEBHOOK_SECRET"),
		OTLPEndpoint:        os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		GitHubToken:         os.Getenv("CAW_GITHUB_TOKEN"),
		GitHubAPIBase:       os.Getenv("CAW_GITHUB_API"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
