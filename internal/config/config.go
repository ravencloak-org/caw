// Package config loads Hub configuration from the environment.
package config

import (
	"log"
	"os"
	"strconv"
	"time"
)

// Defaults for the operational tuning knobs (CONTEXT.md "tuning knobs in
// flight": grace-window duration, rebase-lease TTL/heartbeat). These mirror the
// package-level constants at the call sites (settle.DefaultGrace, hub.leaseTTL,
// rebase.heartbeatInterval) and are the source of truth for the
// unset/invalid-env fallback — so an unset or malformed env var reproduces
// today's exact behavior with zero drift.
const (
	defaultSettleGrace           = 30 * time.Second
	defaultRebaseLeaseTTLSeconds = int64(90)
	defaultRebaseHeartbeat       = 30 * time.Second
)

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
	// BootstrapToken is the operator secret that gates the GitHub App manifest
	// routes (CAW_BOOTSTRAP_TOKEN). Without it the manifest flow is disabled.
	// It is effectively single-use: once App credentials exist, the manifest
	// routes refuse to overwrite them unless AllowRebootstrap is set.
	BootstrapToken string
	// AllowRebootstrap permits the manifest flow to overwrite credentials that
	// already exist (ALLOW_REBOOTSTRAP). Off by default so a leaked bootstrap
	// token cannot replace a live App's credentials.
	AllowRebootstrap bool
	// AppSlug overrides the GitHub App slug used to build the install-redirect
	// URL on the Auth v2 zero-installations branch (Phase 3). Defaults to "";
	// when empty, the hub falls back to store.AnyAppSlug (populated from the
	// installation.created webhook). Operator-side env var: CAW_APP_SLUG.
	AppSlug string

	// Operational tuning knobs (CONTEXT.md). Each defaults to the current
	// hardcoded value, so leaving the env var unset is a no-op.

	// SettleGrace is the settle grace window after the latest trigger
	// (CAW_SETTLE_GRACE, a Go duration like "30s"). Default 30s.
	SettleGrace time.Duration
	// RebaseLeaseTTLSeconds is the store-level rebase-lease TTL in seconds,
	// shared by the Hub lease handler and the orphan rebase handler
	// (CAW_REBASE_LEASE_TTL, an integer count of seconds). Default 90.
	RebaseLeaseTTLSeconds int64
	// RebaseHeartbeat is how often a rebase session/orphan handler renews its
	// lease (CAW_REBASE_HEARTBEAT, a Go duration like "30s"). Must stay
	// comfortably shorter than RebaseLeaseTTLSeconds. Default 30s.
	RebaseHeartbeat time.Duration
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
		BootstrapToken:      os.Getenv("CAW_BOOTSTRAP_TOKEN"),
		AllowRebootstrap:    getenvBool("ALLOW_REBOOTSTRAP"),
		AppSlug:             os.Getenv("CAW_APP_SLUG"),

		SettleGrace:           getenvDuration("CAW_SETTLE_GRACE", defaultSettleGrace),
		RebaseLeaseTTLSeconds: getenvInt64("CAW_REBASE_LEASE_TTL", defaultRebaseLeaseTTLSeconds),
		RebaseHeartbeat:       getenvDuration("CAW_REBASE_HEARTBEAT", defaultRebaseHeartbeat),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getenvBool reports whether the environment variable key is set to a truthy
// value ("1" or "true", case-insensitively).
func getenvBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "True":
		return true
	default:
		return false
	}
}

// getenvDuration parses key as a Go duration (e.g. "30s", "2m"). An unset,
// empty, unparseable, or non-positive value falls back to def — and a malformed
// value is logged rather than crashing the Hub, since a bad tuning knob must not
// take the process down on boot.
func getenvDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("config: %s=%q is not a valid duration (%v); using default %s", key, raw, err, def)
		return def
	}
	if d <= 0 {
		log.Printf("config: %s=%q must be positive; using default %s", key, raw, def)
		return def
	}
	return d
}

// getenvInt64 parses key as a base-10 int64. An unset, empty, unparseable, or
// non-positive value falls back to def, logging malformed input rather than
// crashing the Hub.
func getenvInt64(key string, def int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		log.Printf("config: %s=%q is not a valid integer (%v); using default %d", key, raw, err, def)
		return def
	}
	if n <= 0 {
		log.Printf("config: %s=%q must be positive; using default %d", key, raw, def)
		return def
	}
	return n
}
