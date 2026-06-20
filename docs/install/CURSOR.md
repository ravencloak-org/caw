# Install caw-watcher for Cursor

Auth v2 makes this almost stateless — install the binary, point at the hub,
and let the `login` MCP tool fetch your token over a browser-driven OAuth
handshake. No copy/paste, no env-var token to rotate.

## 1. Download

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

## 2. Configure Cursor

Cursor uses the same MCP config schema as Claude Desktop. Edit (or create)
`~/.cursor/mcp.json` and add the `caw` MCP server. **No token env var** —
`caw-watcher` discovers the token from `~/.config/caw/credentials.json` after
step 3:

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

Restart Cursor so it picks up the new MCP server, then in a session invoke the
`login` tool:

```
login()
```

Your browser opens to `${CAW_WATCHER_HUB_URL}/auth/u/<session_id>`. Authorize
the caw GitHub App, pick installation(s), and the hub delivers a token bound
to your GitHub user + installation(s) over a one-shot loopback POST back to
the watcher. It writes `~/.config/caw/credentials.json` (mode `0600`);
subsequent tool calls find it automatically.

For sandboxed environments (Codespaces, locked-down containers), pass
`force_device: true` and the watcher uses GitHub-style device-code polling
instead.

See [`MCP-LOGIN.md`](./MCP-LOGIN.md) for the full login walkthrough, error
recovery, and how to rotate a leaked token.

## 4. Smoke test

```
subscribe_pr(owner: "<org>", repo: "<repo>", number: 1)
```

A Summary returns within ~30 seconds. After this first subscribe, any PR you
raise from this machine auto-subscribes via the per-user control stream — no
second `subscribe_pr` call needed.
