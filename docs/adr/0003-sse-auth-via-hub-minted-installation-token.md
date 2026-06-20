# 0003 — SSE auth via a Hub-minted per-installation token

**Status:** Accepted (per-user-isolation clause superseded by [ADR-0011](./0011-user-bound-installation-tokens-and-mcp-login.md))
**Date:** 2026-06-07
**Superseded-By:** [ADR-0011](./0011-user-bound-installation-tokens-and-mcp-login.md) (per-user-isolation clause only — all other clauses stand)

## Decision

A Watcher authenticates to the Hub (for `subscribe_pr`, `get_pending`, `ack`) with a **Hub-minted token bound to a GitHub App installation**, issued once at install time — SaaS: when the user clicks *Install*; self-host: when the Manifest flow provisions their App. On each call the Hub checks that the requested `owner/repo` belongs to that token's installation. The Hub already holds the `installation → repos` mapping because it receives those repos' webhooks, so the check is local.

The isolation boundary is **the installation**: any member of an installation may subscribe to any PR stream under it. There is no per-user isolation *within* an installation in v1.

## Why

- The installation is the credential the Hub **already trusts** — webhooks only arrive for installed repos — so authorization is a local lookup, no extra GitHub API round-trip per subscribe.
- No customer PATs stored (consistent with the rest of the design); the token is the Hub's own, fully revocable server-side.
- **Harness-agnostic:** it is GitHub-derived identity carried as a bearer token, not a Claude/Gemini/OpenAI session concept — so it works for any MCP client.
- Self-host and SaaS stay symmetric: same token-at-install mechanism, different issuer.
- Fan-out (many listeners per PR, see ADR-0002 amendment) removed the need for *per-PR exclusivity*, collapsing SSE auth to a pure repo-scoped visibility check — which an installation token answers directly.

## Trade-offs accepted

- One more secret in the user's Watcher/MCP config. Acceptable: it is dropped in during the one-time install step the user already performs.
- Installation-wide visibility: a sibling session in the same org can listen to any of that org's PR streams. Acceptable under fan-out — the threat model protects against strangers, not same-installation siblings.

## Considered alternatives

- **GitHub user OAuth (device flow) on the Watcher:** rejected for v1 — adds an OAuth dance to every Watcher and a per-subscribe API round-trip, in exchange for per-user (not per-installation) granularity that v1 does not need. Revisit if intra-org user isolation becomes a requirement.
- **Self-asserted per-session ID as the ownership key:** rejected — a session ID is a name, not a credential, and is harness-specific. See CONTEXT.md → *Flagged ambiguities*.
- **Per-PR exclusive token:** unnecessary once subscriptions fan out; nothing to make exclusive.
