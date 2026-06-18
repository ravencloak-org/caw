// Package watcher — credentials file management for Auth v2 MCP login (Phase 3).
//
// The credentials file is the canonical store for tokens minted via the
// `login` MCP tool. Schema is JSON, version-tagged, and the file lives at
// ~/.config/caw/credentials.json (XDG-respecting; we use ConfigDir which on
// macOS returns ~/Library/Application Support, on linux ~/.config; we
// document the macOS path explicitly).
//
// Permissions are 0600. Writes are atomic (write to a tmpfile in the same
// directory, then rename) so a crash mid-write never leaves the file in a
// half-decoded state for the next Load().
package watcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CredentialsVersion is the schema version written to every credentials file.
// Bump it (and add a migration path in Load) on any breaking change.
const CredentialsVersion = 1

// Credentials is the JSON shape of credentials.json on disk.
type Credentials struct {
	Version         int           `json:"version"`
	HubURL          string        `json:"hub_url"`
	GitHubUserID    int64         `json:"github_user_id"`
	GitHubUserLogin string        `json:"github_user_login"`
	Tokens          []TokenRecord `json:"tokens"`
}

// TokenRecord is one entry in credentials.json's Tokens slice.
type TokenRecord struct {
	InstallationID string `json:"installation_id"`
	Org            string `json:"org"`
	Token          string `json:"token"`
	TokenID        string `json:"token_id"`
	DeviceLabel    string `json:"device_label,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
}

// DefaultCredentialsPath returns the canonical credentials.json path:
// $XDG_CONFIG_HOME/caw/credentials.json (or ~/.config/caw/... if XDG unset).
// On macOS Go's os.UserConfigDir returns ~/Library/Application Support; the
// plan picks ONE path per host OS and documents it — we honor that.
func DefaultCredentialsPath() (string, error) {
	if env := os.Getenv("CAW_CREDENTIALS_PATH"); env != "" {
		return env, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("user config dir: %w", err)
	}
	return filepath.Join(dir, "caw", "credentials.json"), nil
}

// LoadCredentials reads credentials.json from path. Returns ok=false on a
// missing file (a fresh MCP install has no credentials yet). All other read
// failures (parse error, EOF mid-file) return an error rather than masking
// as ok=false — a corrupt credentials.json is operator-visible noise.
func LoadCredentials(path string) (Credentials, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, false, nil
		}
		return Credentials{}, false, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, false, fmt.Errorf("decode credentials: %w", err)
	}
	if c.Version != CredentialsVersion {
		return Credentials{}, false, fmt.Errorf("credentials version %d unsupported (want %d)",
			c.Version, CredentialsVersion)
	}
	return c, true, nil
}

// SaveCredentials writes c to path atomically with 0600 permissions:
//
//  1. mkdir -p the parent directory (mode 0700).
//  2. Write to a sibling tmp file with mode 0600.
//  3. Rename the tmp file over the target — atomic on POSIX file systems.
//
// The atomic rename matters: a partial write (process killed mid-flush)
// must not leave the next Load() facing half-JSON.
func SaveCredentials(path string, c Credentials) error {
	if c.Version == 0 {
		c.Version = CredentialsVersion
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir credentials dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credentials.*.tmp")
	if err != nil {
		return fmt.Errorf("create credentials tmpfile: %w", err)
	}
	tmpPath := tmp.Name()
	// Cleanup tmpfile on any error path below.
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod credentials tmpfile: %w", err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close credentials tmpfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename credentials: %w", err)
	}
	return nil
}

// ClearCredentials removes the credentials file. A missing file is not an
// error — `logout` is naturally idempotent.
func ClearCredentials(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove credentials: %w", err)
	}
	return nil
}

// ResolveTokenForInstallation returns the token bound to installationID, or
// ("",false) if none. Used by the watcher.Client when it needs to authenticate
// a hub call against a specific installation.
func (c *Credentials) ResolveTokenForInstallation(installationID string) (string, bool) {
	for _, t := range c.Tokens {
		if t.InstallationID == installationID {
			return t.Token, true
		}
	}
	return "", false
}

// FirstToken returns the first token in the file, or ("",false) if empty. The
// MCP's pre-`login` env-only path used a single CAW_WATCHER_TOKEN; this is the
// equivalent fallback for unscoped hub calls (e.g. /pending, which the hub
// scopes by the token's installation anyway).
func (c *Credentials) FirstToken() (string, bool) {
	if len(c.Tokens) == 0 {
		return "", false
	}
	return c.Tokens[0].Token, true
}
