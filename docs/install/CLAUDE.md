# Install caw-watcher for Claude Desktop

Auth v2 makes this almost stateless — install the binary, point at the hub,
and let the `login` MCP tool fetch your token over a browser-driven OAuth
handshake. No copy/paste, no env-var token to rotate.

## 1. Download

Grab the right `caw-watcher` build for your machine, extract it, and drop it on `PATH`.

```sh
# macOS arm64 (Apple Silicon)
curl -L https://github.com/ravencloak-org/caw/releases/latest/download/caw-watcher-darwin-arm64.tar.gz \
  | tar -xz -C /tmp \
  && sudo mv /tmp/caw-watcher /usr/local/bin/caw-watcher \
  && sudo chmod +x /usr/local/bin/caw-watcher
```

```sh
# linux amd64
curl -L https://github.com/ravencloak-org/caw/releases/latest/download/caw-watcher-linux-amd64.tar.gz \
  | tar -xz -C /tmp \
  && sudo mv /tmp/caw-watcher /usr/local/bin/caw-watcher \
  && sudo chmod +x /usr/local/bin/caw-watcher
```

For other platforms, swap `<OS>` / `<ARCH>` in:

```
https://github.com/ravencloak-org/caw/releases/latest/download/caw-watcher-<OS>-<ARCH>.tar.gz
```

## 2. Configure Claude Desktop

Edit (or create) `~/Library/Application Support/Claude/claude_desktop_config.json`
and add the `caw` MCP server. **No token env var** — `caw-watcher` discovers
the token from `~/.config/caw/credentials.json` after step 3:

```json
{
  "mcpServers": {
    "caw": {
      "command": "/usr/local/bin/caw-watcher",
      "env": {
        "CAW_WATCHER_HUB_URL": "https://caw.ravencloak.org"
      }
    }
  }
}
```

If you self-host, swap `CAW_WATCHER_HUB_URL` for your own Hub's public URL.

## 3. Log in

Restart Claude Desktop so it picks up the new MCP server, then in a session
invoke the `login` tool:

```
login()
```

Your browser opens to `${CAW_WATCHER_HUB_URL}/auth/u/<session_id>`. You authorize
the caw GitHub App, pick which installation(s) this device should see, and the
hub mints a token bound to your GitHub user + the chosen installation(s) and
delivers it straight back to the watcher over a one-shot loopback POST. The
token lands in `~/.config/caw/credentials.json` (mode `0600`); subsequent tool
calls find it automatically.

If your environment cannot host a localhost listener (Codespaces, sandboxed
containers, locked-down corporate workstations), pass `force_device: true` and
the watcher falls back to GitHub-style device-code polling.

See [`MCP-LOGIN.md`](./MCP-LOGIN.md) for the full login walkthrough, error
recovery, and how to rotate a leaked token.

## 4. Smoke test

Once logged in, subscribe to a PR you know has at least one `check_suite` event:

```
subscribe_pr(owner: "<org>", repo: "<repo>", number: 1)
```

A Summary comes back within ~30 seconds. If nothing arrives, run `auth_status`
to confirm the watcher sees a valid token, and `get_pending` to drain any
items queued before login.

After this initial subscribe, any PR you raise from this machine
(`gh pr create`, GitHub UI, your agent's own `gh` calls) auto-subscribes via
the per-user control stream — no second `subscribe_pr` call needed.
