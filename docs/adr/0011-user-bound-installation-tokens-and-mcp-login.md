# 0011 — User-bound installation tokens + MCP-initiated login

**Status:** Accepted
**Date:** 2026-06-20
**Extends:** [ADR-0010](./0010-self-service-watcher-tokens.md) (self-service install callback).
**Supersedes (clause-level):** the *"no per-user isolation within an installation"* clause of [ADR-0003](./0003-sse-auth-via-hub-minted-installation-token.md). All other clauses of ADR-0003 stand — the token shape (32-byte random, SHA-256 hashed in DB, presented as `Authorization: Bearer ...`), the installation as the unit of hub↔repo mapping, and webhooks as the source of `installation→repos` truth.

## Context

Three concrete pain points motivated this revision:

1. **Privacy leakage.** Under ADR-0003 the isolation boundary is the installation: any token holder under an installation could subscribe to any PR stream under it. A leaked token (or a sibling session in the same org installed across `private-thing` AND `secret-thing`) could tail PRs in a repo the holder is not personally a collaborator on. That model fit a one-org-trusted-team threat model; it does not fit "public hub, anyone installs the App."
2. **No MCP-initiated login.** The token had to be minted operator-side (`hub mint-token`) or surfaced via a one-shot HTML page after install, then pasted into a harness env var. Every onboarding step was a chance for the token to land in a shell history file, a screenshot, or a chat log.
3. **No rotation/revocation surface.** Once a token was minted, it was forever. A leaked token could not be killed without `DELETE FROM tokens` against the hub's SQLite, which also broke every other watcher sharing that installation.

ADR-0003 itself invited this revisit: *"Revisit if intra-org user isolation becomes a requirement."* We are now accepting that invitation.

## Decision

Five high-leverage decisions, each implemented across Phases 1-5 of the rollout (see `local://caw-auth-v2-plan.md` for the per-phase detail).

### D1 — Token shape stays opaque + SHA-256 hashed in DB

We keep ADR-0003's `base64url(32 random bytes)` opaque token. Hash with SHA-256, store the hash + lifecycle metadata in `tokens`. Verification = one indexed `SELECT` keyed on hash. We rejected JWT and PASETO: statelessness buys nothing for a hub that already hits SQLite on every request for `RequireRepoScope`, and revocation (the headline new capability of Auth v2) is trivial against an opaque row (`UPDATE tokens SET revoked_at = ?`). Implementation: [`internal/store/store.go`](../../internal/store/store.go) (`Token` row + `InsertTokenRow` / `VerifyToken` / `RevokeToken`).

### D2 — Loopback OAuth handoff (default) + device-code fallback

The MCP plugin (`cmd/watcher`) drives login. Loopback default: the watcher picks a free `127.0.0.1` port, the hub redirects the browser back to it with a one-shot POST carrying the token bundle. Device-code fallback for sandboxed environments (Codespaces, locked-down containers): the user types a short `user_code` into a browser, the watcher polls `/auth/poll`. Both branches are PKCE-bound; loopback verifies the `code_challenge` echo to defend against a rogue process intercepting the loopback port. Implementation: [`internal/hub/auth_session_handler.go`](../../internal/hub/auth_session_handler.go), [`cmd/watcher/main.go`](../../cmd/watcher/main.go).

### D3 — Authoritative per-user repo-access check

`auth.Required` now sets BOTH `installation_id` AND `github_user_id` in `gin.Context`. After it (and the existing cheap `RequireRepoScope` defense-in-depth check), `RequireRepoAccess` calls `GET /repos/{owner}/{repo}/collaborators/{github_user_login}/permission` via an App installation token, cached for 5 min positive / 60 s negative with a 30-min stale-allow grace on GitHub 5xx. Implementation: [`internal/hub/repo_access_middleware.go`](../../internal/hub/repo_access_middleware.go), [`internal/repoaccess`](../../internal/repoaccess).

### D4 — Per-device token lifecycle with rotation + revocation

Tokens carry a `device_label`, `expires_at` (90 days from creation), `last_used_at` (debounced writeback), and `revoked_at`. N concurrent tokens per `(github_user_id, installation_id)`. Revocation paths: MCP `logout`, `/me/tokens` web page (one click per row), `/me/recover` (panic button revoking every token bound to the user), `installation.deleted` webhook (kills every token bound to the gone installation), and operator break-glass `hub revoke-token` / `hub migrate-tokens`. Implementation: [`internal/hub/me_handler.go`](../../internal/hub/me_handler.go), [`cmd/hub/main.go`](../../cmd/hub/main.go).

### D5 — Control-plane stream for transparent auto-subscribe

Each logged-in MCP holds a long-running SSE connection to `/sse/me/control`. When a `pull_request.opened` webhook fires with `sender.id == token.github_user_id`, the hub publishes a `pr_opened` event; the MCP opens a per-PR subscription transparently. The user never has to remember to call `subscribe_pr` after raising a PR. This is the end-user value loop Auth v2 makes possible — Phase 3 made login possible, Phase 3.5 makes the product useful without per-tool ceremony. Implementation: [`internal/sse/control_hub.go`](../../internal/sse/control_hub.go), [`internal/sse/control_handler.go`](../../internal/sse/control_handler.go).

## Wire protocol

Spec lives in `local://caw-auth-v2-plan.md` §"Wire protocol — MCP login handoff" (kept by the operator, shared on request). End-user-facing flow walkthrough: [`docs/install/MCP-LOGIN.md`](../install/MCP-LOGIN.md). Operator runbook: [`docs/install/SELF-HOST.md`](../install/SELF-HOST.md) §6.

## Authorization model

Middleware stack in route order on `/sse/...` and `/leases/...`:

```
[otelgin] → [Logger] → [Recovery] → [auth.Required] → [RequireRepoScope] → [RequireRepoAccess] → handler
```

| Condition                                       | Response                                    |
| ----------------------------------------------- | ------------------------------------------- |
| GitHub permission ≥ read                        | 200 / next                                  |
| GitHub 404 (no access)                          | 404                                         |
| Cache: positive entry within grace + GH 5xx     | 200 / next + `Deprecation: stale-allow`     |
| GitHub 5xx with no usable prior cache entry     | 503 + `Retry-After: 30`                     |
| GitHub 403 (App permissions misconfigured)      | 500 (operator bug; logged)                  |
| Legacy token + `CAW_ALLOW_LEGACY_TOKENS=1`      | 200 / next + `Deprecation: legacy-token`    |
| Legacy token (Phase 5 default)                  | 400 + `{"error":"user-bound token required", "login_url":"/auth/start", "message":"user-bound token required; run \`login\` from your agent"}` |

`/sse/me/control` and `/me/*` use `auth.Required` only — per-user, not per-repo. Both reject legacy tokens at the handler with the same actionable 400.

## Migration

Rolling cutover over three releases:

- **v0.2.0** — Phases 1-4. New columns added (nullable). Legacy tokens (`github_user_id IS NULL`) bypass `RequireRepoAccess` with `Deprecation: legacy-token`. New tokens minted through `/auth/...` are user-bound and enforced.
- **v0.3.0** (this release) — Phase 5 cutover. `RequireRepoAccess` rejects legacy tokens by default with the 400 above. Operators set `CAW_ALLOW_LEGACY_TOKENS=1` to preserve the bypass for one more release of headroom. The `hub migrate-tokens` operator subcommand (idempotent, with `--dry-run`) revokes every active legacy row in one shot.
- **v0.4.0** — `github_user_id` column made `NOT NULL`. Migration drops any remaining legacy rows automatically.

## Alternatives considered

- **JWT signed by the hub** (D1) — rejected. Statelessness buys nothing when the hub already hits SQLite on every request, and instant revocation requires a denylist anyway.
- **Device-code-only handoff** (D2) — rejected. Worse UX on the 80% laptop case where loopback works directly.
- **Per-PR exclusive tokens** — already addressed by `RequireRepoScope`; nothing extra to do.
- **Do nothing; document the trust model honestly** — rejected. The threat model shifted from "one-org trusted team" to "public hub, anyone installs" and a leaked token tailing strangers' OSS PRs is not a stance we can defend.

## Consequences

**New routes:**
- `POST /auth/start`, `GET /auth/u/:session_id`, `GET /auth/cb/github`, `GET /auth/picker/:session_id`, `POST /auth/picker/:session_id`, `GET /auth/device`, `POST /auth/poll`, `GET /auth/done/:session_id`, `GET /auth/start-help`.
- `GET /me`, `GET /me/tokens`, `DELETE /me/tokens/:id`, `POST /me/recover`.
- `GET /sse/me/control`.

**New schema:**
- `tokens.{id, github_user_id, github_user_login, device_label, expires_at, last_used_at, revoked_at}` columns; `tokens_user`, `tokens_installation`, `tokens_expires_active` indexes.
- `auth_sessions` table (PKCE session state).
- `installations.app_slug` column (for redirect-to-install path).

**New env vars:**
- `CAW_APP_SLUG` — override for the App slug used in redirect-to-install (falls back to the per-installation `installations.app_slug` row).
- `CAW_ALLOW_LEGACY_TOKENS` — `1` preserves the pre-cutover bypass for one more release. Default `0` rejects.

**New CLI subcommands:**
- `hub revoke-token <token_id>` — revoke a single token by id.
- `hub migrate-tokens [--dry-run]` — revoke every active legacy row idempotently.

## Follow-ups (out of scope)

- Subscribing to `member.*` / `organization.*` webhooks for faster-than-TTL revocation on org-membership churn. v2's 5-min positive TTL handles it eventually; explicit flush is a flag-gated follow-up.
- Per-team and per-PR-author scoping. v2 only checks repo-read.
- Multi-VCS support (GitLab, Bitbucket, Gitea).
- Refresh-token grants. Tokens are simple-bearer with a generous lifetime; re-login is the renewal channel.
