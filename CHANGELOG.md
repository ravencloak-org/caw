# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- feat(auth-v2): phase 2 — per-user authorization middleware + repo-access cache. Adds `internal/repoaccess` (5-min positive / 60-s negative TTL, 30-min stale-allow grace, single-flight collapse) and `internal/hub/repo_access_middleware.go`. Wired after `RequireRepoScope` on `/sse/...` and `/leases/...`. Legacy tokens (NULL `github_user_id`) bypass with `Deprecation: legacy-token`; enforcement begins in Phase 5. Webhook flush hooks on `installation.deleted` and `installation_repositories.removed`.

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
