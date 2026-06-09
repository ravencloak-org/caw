// Package watcher — runtime configuration loaded from environment variables.
package watcher

import (
	"fmt"
	"os"
)

const (
	// EnvHubURL is the environment variable that specifies the Hub base URL.
	EnvHubURL = "CAW_WATCHER_HUB_URL"
	// EnvToken is the environment variable that holds the Hub-minted installation token (ADR-0003).
	EnvToken = "CAW_WATCHER_TOKEN"
)

// Config holds the watcher's runtime configuration.
type Config struct {
	// HubURL is the base URL of the running Hub (e.g. "http://localhost:8080").
	HubURL string
	// Token is the Hub-minted installation bearer token (ADR-0003).
	Token string
}

// ConfigFromEnv reads Config from environment variables.
// Returns an error if any required variable is missing.
func ConfigFromEnv() (Config, error) {
	hubURL := os.Getenv(EnvHubURL)
	if hubURL == "" {
		return Config{}, fmt.Errorf("watcher: %s is required", EnvHubURL)
	}
	token := os.Getenv(EnvToken)
	if token == "" {
		return Config{}, fmt.Errorf("watcher: %s is required", EnvToken)
	}
	return Config{HubURL: hubURL, Token: token}, nil
}
