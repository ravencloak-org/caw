# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **feat(auth-v2): user-bound installation tokens + MCP-initiated login**
  (issues [#56-#62], PRs [#64-#70] + this PR). Replaces the v0.1.x
  "operator mints, user pastes" token flow with per-user, per-device,
  per-installation tokens issued through a hub-driven OAuth handshake the
  MCP plugin invokes. Highlights:
  - Schema (Phase 1): `tokens.{id, github_user_id, github_user_login,
    device_label, expires_at, last_used_at, revoked_at}` columns +
    `auth_sessions` table + `installations.app_slug`. Online additive
    migration — no downtime.
  - Per-user authorization (Phase 2): `internal/repoaccess` cache
    (5-min positive / 60-s negative TTL, 30-min stale-allow grace,
    single-flight collapse) + `RequireRepoAccess` middleware on
    `/sse/...` and `/leases/...`. Webhook-driven flush on
    `installation.deleted` and `installation_repositories.removed`.
  - MCP-initiated login (Phase 3): `/auth/*` routes (loopback + device
    handshakes, PKCE-bound) + `login` / `logout` / `auth_status` tools on
    `caw-watcher`. Token file at `~/.config/caw/credentials.json` (mode
    `0600`).
  - Auto-subscribe (Phase 3.5): per-user control stream at
    `/sse/me/control`; `pull_request.opened` webhooks fan out to the user
    who opened the PR so the MCP transparently subscribes without a
    `subscribe_pr` call.
  - Token management (Phase 4): `/me`, `/me/tokens`, `DELETE
    /me/tokens/:id`, `/me/recover` — list and revoke per-device. MCP
    `logout` revokes the current device's token; `installation.deleted`
    revokes every token bound to that installation.
  - **Cutover (Phase 5, BREAKING):** `RequireRepoAccess` now rejects
    legacy (NULL `github_user_id`) tokens with `400 user-bound token
    required; run \`login\` from your agent`. Operators set
    `CAW_ALLOW_LEGACY_TOKENS=1` to preserve the bypass for one more
    release of headroom. New `hub migrate-tokens` subcommand (with
    `--dry-run`) revokes every active legacy row idempotently. Install
    docs (`docs/install/CLAUDE.md` / `CURSOR.md` / `CODEX-CLI.md`)
    rewritten to point at the `login` tool; new `docs/install/MCP-LOGIN.md`
    covers the end-user flow; `docs/install/SELF-HOST.md` extended with
    the operator runbook. ADR-0011 captures the design;
    [ADR-0003](./docs/adr/0003-sse-auth-via-hub-minted-installation-token.md)'s
    "no per-user isolation within an installation" clause is superseded.

### Breaking

- Legacy tokens minted via `hub mint-token` before Auth v2 are now rejected
  on `/sse/...` and `/leases/...`. Set `CAW_ALLOW_LEGACY_TOKENS=1` for one
  release of headroom, run `hub migrate-tokens` to revoke them, and have
  developers re-login via the `login` MCP tool.

## [v0.1.4]

### Changed

- ci(release): create GitHub release with auto-notes on tag ([#38](https://github.com/ravencloak-org/caw/pull/38))
- ci(release): drop QEMU + arm64 + GHA cache, native amd64 only ([#45](https://github.com/ravencloak-org/caw/pull/45))

## [v0.1.3]

### Changed

- chore(severity): cut dead adapter/registry surface ([#37](https://github.com/ravencloak-org/caw/pull/37))

## [v0.1.2]

### Changed

- ci(release): build to GHCR + Dokploy webhook redeploy on green CI ([#36](https://github.com/ravencloak-org/caw/pull/36))

## [v0.1.1]

### Changed

- ci(release): build, push to GHCR, redeploy via Dokploy webhook
- ci(release): disable checkout credential persistence
- chore(deps): bump `docker/build-push-action` from 6 to 7 ([#30](https://github.com/ravencloak-org/caw/pull/30))
- chore(deps): bump `actions/setup-python` from 5 to 6 ([#31](https://github.com/ravencloak-org/caw/pull/31))
- chore(deps): bump `docker/setup-qemu-action` from 3 to 4 ([#34](https://github.com/ravencloak-org/caw/pull/34))
- chore(deps): bump `codecov/codecov-action` from 5 to 7 ([#32](https://github.com/ravencloak-org/caw/pull/32))
- chore(deps): bump `docker/metadata-action` from 5 to 6 ([#33](https://github.com/ravencloak-org/caw/pull/33))
- chore(schema): bump pinned dolt 2.1.2 → 2.1.4 ([#35](https://github.com/ravencloak-org/caw/pull/35))
- docs: update README to reflect implemented, deployed state ([#29](https://github.com/ravencloak-org/caw/pull/29))

## [v0.1.0]

First tagged release of the **caw** Hub. caw lets a coding agent subscribe over SSE
to its own PR after raising it — the Hub pushes compiled PR + CI/action status,
review comments (incl. CodeRabbit), and mergeability to the agent's Watcher,
killing the `gh` polling loop.

This release publishes the first multi-arch Docker image to GHCR:

- `ghcr.io/ravencloak-org/caw:v0.1.0`
- `ghcr.io/ravencloak-org/caw:0.1`
- `ghcr.io/ravencloak-org/caw:latest`

### Added

- Webhook ingest + HMAC verify + Round bucketing + SQLite store
- Compile + SSE fan-out, settle/re-settle, installation-token auth, `get_pending`
- Mergeability poll; severity ladder + pluggable review adapters (CodeRabbit)
- Watcher MCP (`subscribe_pr` / `get_pending` / `acquire_rebase_lease`)
- GitHub App identity via the manifest flow + per-installation tokens (also drive the mergeability poll + auto-merge)
- Session + orphan rebase and auto-merge under a hub-granted lease
- OpenTelemetry → bundled OpenObserve observability
- Dolt as versioned schema source-of-truth (sqlite + postgres DDL)
- Portable Go + embedded SQLite, single distroless static binary; self-host via `docker compose` (ADR-0001)

[Unreleased]: https://github.com/ravencloak-org/caw/compare/v0.1.4...HEAD
[v0.1.4]: https://github.com/ravencloak-org/caw/releases/tag/v0.1.4
[v0.1.3]: https://github.com/ravencloak-org/caw/releases/tag/v0.1.3
[v0.1.2]: https://github.com/ravencloak-org/caw/releases/tag/v0.1.2
[v0.1.1]: https://github.com/ravencloak-org/caw/releases/tag/v0.1.1
[v0.1.0]: https://github.com/ravencloak-org/caw/releases/tag/v0.1.0
