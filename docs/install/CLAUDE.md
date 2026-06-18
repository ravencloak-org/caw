# Install caw-watcher for Claude Desktop

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

Edit (or create) `~/Library/Application Support/Claude/claude_desktop_config.json` and add the `caw` MCP server:

```json
{
  "mcpServers": {
    "caw": {
      "command": "/usr/local/bin/caw-watcher",
      "env": {
        "CAW_WATCHER_HUB_URL": "https://caw.ravencloak.org",
        "CAW_WATCHER_TOKEN": "<paste token from `hub mint-token`>"
      }
    }
  }
}
```

If you self-host, swap `CAW_WATCHER_HUB_URL` for your own Hub's public URL.

## 3. Get a Hub token

- **Self-hosters:** mint a token against your own Hub:

  ```sh
  docker compose exec hub /hub mint-token <installation_id> <org>
  ```

  `<installation_id>` is the GitHub App installation; `<org>` is the org or user that installed it.

- **Public Hub (`caw.ravencloak.org`):** install the [caw GitHub App](https://github.com/apps/caw-ravencloak) on the repo you want watched. GitHub redirects you to `https://caw.ravencloak.org/github/app/install/callback`, which shows your token once — copy it into the `CAW_WATCHER_TOKEN` env var above ([ADR-0010](../adr/0010-self-service-watcher-tokens.md)).

## 4. Smoke test

1. Fully quit and restart Claude Desktop so it re-reads the config and spawns `caw-watcher`.
2. In a session, call the tool against a PR you know has at least one `check_suite` event:

   ```
   subscribe_pr(owner: "<org>", repo: "<repo>", number: 1)
   ```

3. Confirm a Summary comes back within ~30 seconds. If nothing arrives, double-check `CAW_WATCHER_TOKEN` and that the Hub sees webhooks for that PR.
