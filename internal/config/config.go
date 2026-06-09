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

	// GitHub App fields (Slice 5).

	// AppID is the numeric GitHub App ID (CAW_APP_ID).
	AppID string
	// AppPrivateKeyPath is the path to the RSA private key PEM file (CAW_APP_PRIVATE_KEY_PATH).
	// Mutually exclusive with AppPrivateKeyPEM.
	AppPrivateKeyPath string
	// AppPrivateKeyPEM is the RSA private key PEM, inline (CAW_APP_PRIVATE_KEY_PEM).
	// Mutually exclusive with AppPrivateKeyPath.
	AppPrivateKeyPEM string
	// AppClientID is the GitHub App OAuth client ID (CAW_APP_CLIENT_ID).
	AppClientID string
	// AppClientSecret is the GitHub App OAuth client secret (CAW_APP_CLIENT_SECRET).
	AppClientSecret string
	// BaseURL is the publicly reachable URL of this Hub (CAW_BASE_URL).
	// Used to construct the redirect_url in the manifest flow.
	BaseURL string
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
		AppID:               os.Getenv("CAW_APP_ID"),
		AppPrivateKeyPath:   os.Getenv("CAW_APP_PRIVATE_KEY_PATH"),
		AppPrivateKeyPEM:    os.Getenv("CAW_APP_PRIVATE_KEY_PEM"),
		AppClientID:         os.Getenv("CAW_APP_CLIENT_ID"),
		AppClientSecret:     os.Getenv("CAW_APP_CLIENT_SECRET"),
		BaseURL:             os.Getenv("CAW_BASE_URL"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
