// Auth v2 Phase 3 — login / logout / auth_status MCP tool handlers.
//
// These are wired into newServer() in main.go. They live in a separate file
// so the original tool set (subscribe_pr, get_pending, lease tools) stays
// readable; everything below is new surface introduced by the Auth v2 wire
// protocol (issue #59).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ravencloak-org/caw/internal/watcher"
)

// loginInput is the tool's JSON input shape. force_device opts into the
// device-code flow even when the watcher is on a host with a usable browser
// (Codespaces, locked-down workstations, SSH'd dev boxes).
type loginInput struct {
	ForceDevice bool   `json:"force_device,omitempty" jsonschema:"force the device-code flow even when loopback would work"`
	HubURL      string `json:"hub_url,omitempty"      jsonschema:"override the configured hub URL (defaults to CAW_WATCHER_HUB_URL)"`
}

// loginOutput exposes the user-visible fields of the resulting credentials
// file. Token values themselves are never returned to the model — the model
// has no business holding them.
type loginOutput struct {
	GitHubUserLogin string   `json:"github_user_login"`
	GitHubUserID    int64    `json:"github_user_id"`
	Installations   []string `json:"installations"`
	Mode            string   `json:"mode"`
	CredentialsPath string   `json:"credentials_path"`
}

// makeLogin returns the MCP handler for the `login` tool.
func makeLogin(client *watcher.Client) mcp.ToolHandlerFor[loginInput, loginOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in loginInput) (*mcp.CallToolResult, loginOutput, error) {
		hubURL := strings.TrimRight(in.HubURL, "/")
		if hubURL == "" {
			hubURL = client.HubURL()
		}
		credsPath, err := watcher.DefaultCredentialsPath()
		if err != nil {
			return nil, loginOutput{}, fmt.Errorf("login: resolve credentials path: %w", err)
		}

		// Idempotency: a usable creds file for this hub short-circuits with
		// "already logged in".
		if c, ok, _ := watcher.LoadCredentials(credsPath); ok && c.HubURL == hubURL && len(c.Tokens) > 0 {
			out := loginOutput{
				GitHubUserLogin: c.GitHubUserLogin,
				GitHubUserID:    c.GitHubUserID,
				Installations:   installationsFromCreds(c),
				Mode:            "cached",
				CredentialsPath: credsPath,
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Already logged in to %s as %s (%d tokens). Run logout first to re-authenticate.",
					hubURL, c.GitHubUserLogin, len(c.Tokens)),
			}}}, out, nil
		}

		opts := watcher.LoginOptions{
			HubURL:          hubURL,
			ClientLabel:     defaultClientLabel(),
			CredentialsPath: credsPath,
		}

		mode := "loopback"
		var bundle watcher.TokenBundle
		if in.ForceDevice {
			mode = "device"
			bundle, err = watcher.LoginDevice(ctx, opts)
		} else {
			bundle, err = watcher.LoginLoopback(ctx, opts)
		}
		if err != nil {
			return nil, loginOutput{}, err
		}

		c, _, _ := watcher.LoadCredentials(credsPath)
		out := loginOutput{
			GitHubUserLogin: bundle.GitHubUserLogin,
			GitHubUserID:    bundle.GitHubUserID,
			Installations:   installationsFromCreds(c),
			Mode:            mode,
			CredentialsPath: credsPath,
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("✓ Logged in to %s as %s; minted %d token(s).",
				hubURL, bundle.GitHubUserLogin, len(bundle.Tokens)),
		}}}, out, nil
	}
}

// logoutInput / logoutOutput follow the same shape as login but only carry
// the optional hub override.
type logoutInput struct {
	HubURL string `json:"hub_url,omitempty" jsonschema:"override the configured hub URL"`
}

type logoutOutput struct {
	Cleared         bool   `json:"cleared"`
	CredentialsPath string `json:"credentials_path"`
}

func makeLogout(client *watcher.Client) mcp.ToolHandlerFor[logoutInput, logoutOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in logoutInput) (*mcp.CallToolResult, logoutOutput, error) {
		hubURL := strings.TrimRight(in.HubURL, "/")
		if hubURL == "" {
			hubURL = client.HubURL()
		}
		credsPath, err := watcher.DefaultCredentialsPath()
		if err != nil {
			return nil, logoutOutput{}, fmt.Errorf("logout: resolve credentials path: %w", err)
		}
		if err := watcher.Logout(ctx, hubURL, credsPath, &http.Client{Timeout: 15 * time.Second}); err != nil {
			return nil, logoutOutput{}, err
		}
		out := logoutOutput{Cleared: true, CredentialsPath: credsPath}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Logged out from %s. Run login to re-authenticate.", hubURL),
		}}}, out, nil
	}
}

// authStatusInput is empty.
type authStatusInput struct{}

// authStatusOutput holds the safe fields of the credentials file — never the
// token values themselves.
type authStatusOutput struct {
	LoggedIn        bool             `json:"logged_in"`
	HubURL          string           `json:"hub_url,omitempty"`
	GitHubUserID    int64            `json:"github_user_id,omitempty"`
	GitHubUserLogin string           `json:"github_user_login,omitempty"`
	Tokens          []authStatusItem `json:"tokens"`
	CredentialsPath string           `json:"credentials_path"`
}

type authStatusItem struct {
	InstallationID string `json:"installation_id"`
	Org            string `json:"org"`
	TokenID        string `json:"token_id"`
	DeviceLabel    string `json:"device_label,omitempty"`
	ExpiresAt      int64  `json:"expires_at,omitempty"`
	Expired        bool   `json:"expired,omitempty"`
}

func makeAuthStatus(_ *watcher.Client) mcp.ToolHandlerFor[authStatusInput, authStatusOutput] {
	return func(_ context.Context, _ *mcp.CallToolRequest, _ authStatusInput) (*mcp.CallToolResult, authStatusOutput, error) {
		credsPath, err := watcher.DefaultCredentialsPath()
		if err != nil {
			return nil, authStatusOutput{}, fmt.Errorf("auth_status: resolve credentials path: %w", err)
		}
		c, ok, err := watcher.LoadCredentials(credsPath)
		if err != nil {
			return nil, authStatusOutput{}, err
		}
		if !ok {
			out := authStatusOutput{LoggedIn: false, CredentialsPath: credsPath, Tokens: []authStatusItem{}}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
				Text: "Not logged in. Run the `login` tool to authenticate.",
			}}}, out, nil
		}
		now := time.Now().Unix()
		items := make([]authStatusItem, 0, len(c.Tokens))
		for _, t := range c.Tokens {
			items = append(items, authStatusItem{
				InstallationID: t.InstallationID,
				Org:            t.Org,
				TokenID:        t.TokenID,
				DeviceLabel:    t.DeviceLabel,
				ExpiresAt:      t.ExpiresAt,
				Expired:        t.ExpiresAt > 0 && t.ExpiresAt <= now,
			})
		}
		out := authStatusOutput{
			LoggedIn:        true,
			HubURL:          c.HubURL,
			GitHubUserID:    c.GitHubUserID,
			GitHubUserLogin: c.GitHubUserLogin,
			Tokens:          items,
			CredentialsPath: credsPath,
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Logged in to %s as %s — %d installation(s).",
				c.HubURL, c.GitHubUserLogin, len(c.Tokens)),
		}}}, out, nil
	}
}

func installationsFromCreds(c watcher.Credentials) []string {
	out := make([]string, 0, len(c.Tokens))
	for _, t := range c.Tokens {
		label := t.InstallationID
		if t.Org != "" {
			label = t.Org + " (id " + t.InstallationID + ")"
		}
		out = append(out, label)
	}
	return out
}

func defaultClientLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return "caw-watcher @ " + host
}
