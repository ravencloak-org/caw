# MCP Login — the user-facing `login` flow

Auth v2 replaces the v0.1.x "paste this token into your harness env" flow
with an MCP-initiated browser handshake. This document covers the end-to-end
flow, the credentials file, troubleshooting, and how to rotate a leaked
token. The harness-specific config glue lives in
[`CLAUDE.md`](./CLAUDE.md) / [`CURSOR.md`](./CURSOR.md) /
[`CODEX-CLI.md`](./CODEX-CLI.md).

## The flow at a glance

```
agent ──login()──▶ watcher ──POST /auth/start──▶ hub
                       │  (loopback listener on 127.0.0.1)
                       ▼
                    browser ─▶ GitHub OAuth ─▶ install picker ─▶ hub
                       │
                       ▼  one-shot loopback POST
                    watcher ─▶ ~/.config/caw/credentials.json (mode 0600)
```

1. The agent (Claude / Cursor / Codex CLI) invokes the watcher's `login`
   tool. The watcher opens a one-shot HTTP listener on a free `127.0.0.1`
   port, generates a PKCE `code_verifier` + `code_challenge`, and POSTs
   `/auth/start` to the hub.
2. The hub returns a `session_id` + `verification_url`. The watcher opens
   that URL in your browser (and prints it to stderr in case the launcher
   fails). The browser authorizes the caw GitHub App on your account.
3. If you have not yet installed the App on any repos, the hub redirects you
   to `https://github.com/apps/<slug>/installations/new` and resumes your
   session after install. If you already have installations, the hub renders
   a picker so you choose which installation(s) this device should see.
4. The hub mints one user-bound token row per chosen installation and POSTs
   them back to the watcher's loopback listener. The watcher writes them to
   `~/.config/caw/credentials.json` (mode `0600`) and the browser shows
   "✓ Logged in".

After login, the watcher routes per-repo requests to the right installation
automatically. You never type a token; you never copy one out of a web page.

## The credentials file

`~/.config/caw/credentials.json` (XDG-respecting; macOS defaults to the same
path when XDG vars are unset):

```json
{
  "version": 1,
  "users": [
    {
      "github_user_id": 12345,
      "github_user_login": "jobinlawrance",
      "device_label": "Claude Code @ jobin-mbp",
      "tokens": [
        { "installation_id": "139674548", "owner_login": "ravencloak-org",
          "token_id": "01HK…", "token": "<raw>", "expires_at": 1717000000 }
      ]
    }
  ]
}
```

- **File mode `0600`** — only your user can read it.
- **`device_label`** identifies which physical device this token belongs to
  on `/me/tokens`; it is what you click to revoke the token if the device is
  lost or stolen.
- **`expires_at`** is a 90-day rolling lifetime. The watcher nudges you
  ~7 days before expiry via the `X-Caw-Token-Renew: soon` response header
  the hub stamps on every successful request.

## Tools the watcher exposes

| Tool          | What it does                                              |
| ------------- | --------------------------------------------------------- |
| `login`       | Runs the handshake. Optional `force_device: true` flag.   |
| `logout`      | Revokes the current device's token via `DELETE /me/tokens/<self>` and removes it from the credentials file. |
| `auth_status` | Returns who the hub thinks you are, list of installations, current token expiry. |

## Sandboxed environments — device-code fallback

If your harness cannot host a localhost listener (Codespaces, sandboxed
containers, locked-down corporate workstations), pass `force_device: true`:

```
login(force_device: true)
```

The watcher prints a short `user_code` (e.g. `WDJB-MJHT`) plus a verification
URL. Open the URL on any browser-equipped device, type the code, complete
the OAuth dance, and the watcher's poller picks up the token from
`/auth/poll` within a few seconds.

## Multi-installation handling

If your GitHub user has the caw App installed across several orgs, the
picker shows them all and you tick the ones this device should see. The
watcher stores one row per ticked installation; when you call
`subscribe_pr(owner, repo, number)` the watcher routes to the right token
based on the installation that owns that repo. New installations added
later trigger an `installation_added` control-stream event so the watcher
refreshes without re-login.

## Troubleshooting

- **"login canceled" after 5 min** — the browser tab was closed before the
  picker submission. Re-invoke `login`.
- **"port already bound, falling back to device flow"** — five attempts at
  random localhost ports all hit something else listening. Use
  `force_device: true` or free the ports.
- **`401 Bearer error="legacy_token"` on every tool call** — your credentials
  file holds a pre-Auth-v2 token. Run `logout` then `login` to mint a
  user-bound token.
- **`400 user-bound token required; run \`login\` from your agent`** — same
  cause as above; the cutover started rejecting legacy tokens. Re-login.
- **GitHub OAuth `code` expired** — restart `login`. Codes are single-use
  and live ~10 min.
- **`auth_status` says wrong user** — `logout`, then `login` on the right
  GitHub account.

## Rotating a leaked token

1. Open `https://<hub>/me/tokens` in your browser (re-do OAuth if asked).
2. Find the row matching the leaked device by its `device_label`.
3. Click *Revoke*. The token is dead instantly server-side; the offending
   device's next request returns `401`.
4. Run `login` again on the trusted device(s) to mint fresh tokens — old
   sibling tokens on other devices keep working unrelated to this one.

For a panic-button "kill every token I own", `POST /me/recover` does the
same thing for every row bound to your `github_user_id`, then walks you
through a fresh login.

## See also

- [`SELF-HOST.md`](./SELF-HOST.md) — operator-side setup, App registration,
  and the first-end-user walkthrough.
- [ADR-0011](../adr/0011-user-bound-installation-tokens-and-mcp-login.md) —
  the design decisions behind this flow.
- [ADR-0003](../adr/0003-sse-auth-via-hub-minted-installation-token.md) —
  the per-installation auth shape that ADR-0011 builds on.
