# Caw

> Push PR feedback to the agent that raised the PR — so it fixes its own PR without being prodded.

When an AI agent raises a pull request, getting it green (CI, review comments, mergeability) usually means a human babysitting the PR and re-prompting the agent every time something fails. Caw inverts that: GitHub webhooks are compiled into a single summary and **pushed** to the agent over a held-open SSE connection, so the agent that raised the PR keeps working it on its own. If that agent is gone, the summary waits as a *pending item* for the next agent to pick up.

Caw is **harness-agnostic** (any MCP client — Claude, Gemini, OpenAI, Cursor) and **reviewer-agnostic** (any commenting bot or human — CodeRabbit, Sonar, Snyk, …).

See [`CONTEXT.md`](./CONTEXT.md) for the canonical glossary and [`docs/adr/`](./docs/adr/) for the load-bearing decisions.

## How it works

```
GitHub ──webhooks──▶ Hub ──compile──▶ SSE push ──▶ Watcher ──▶ Session (listening agent)
                      │                                              fixes in worktree, pushes
                      └─ no listener? store as Pending item ──▶ next Session's startup check ──▶ prompt human
```

**Live path.** A **Session** raises a PR; its **Watcher** (a local MCP server) opens and *holds* an SSE connection to the **Hub** keyed `owner/repo#number`. Many Sessions may subscribe to the same PR — summaries **fan out** to all listeners. The Hub ingests that **Round**'s GitHub webhooks; once checks settle (`check_suite completed` + ~30s grace) it runs the one poll (mergeability), compiles a summary, and pushes it down the held connections. A late same-SHA signal (async review comment, flipped check) **re-settles** the Round and pushes a follow-up (one summary *per settle* — [ADR-0004](./docs/adr/0004-rounds-re-settle-on-late-same-sha-signals.md)). The Session acts in the PR's worktree, pushes, and the next Round begins. A new head SHA supersedes the prior Round.

**Catch-up path.** Zero live Subscriptions → the summary is stored as a **Pending item** (latest-state per signal-type — [ADR-0006](./docs/adr/0006-pending-is-latest-state-per-type-consumer-owns-relevance.md)). A brand-new Session makes **one** request at startup ("anything pending?"), surfaced by a SessionStart hook, gets *all* current items, and prompts the human before acting on work it didn't start.

**The only poll in the whole system** is the mergeability/conflict re-verify after a Round settles. Everything else is webhook push (GitHub → Hub) and SSE push (Hub → Watcher). The startup pending-check is a single one-shot request, not polling.

## Signal-types

Feedback is surfaced as three dynamic signal-types; sources within each are attributed at runtime, never hardcoded.

| Signal-type | GitHub source | Notes |
|---|---|---|
| **Checks** | `check_suite` failures | CI, Squawk-as-check, Sonar-as-check… |
| **Comments** | `pull_request_review`, `pull_request_review_comment`, `issue_comment` | Any bot or human; severity normalised for first-class sources (CodeRabbit first) |
| **Mergeability** | poll of `GET /pulls/{n}` after settle | Behind-base & clean → rebase under a Hub-granted single-owner **rebase lease** (listener, or Hub for orphans — [ADR-0005](./docs/adr/0005-hub-granted-rebase-lease.md)); then auto-merge |

Severity renders as **label + symbol + colour** (e.g. `■ CRITICAL` / `▲ MAJOR` / `· minor`), never colour alone — readable on colourblind-accessible terminal themes and on no-colour terminals.

**Mapping onto the ladder.** The severity ladder is **closed** (a fixed 3–4 levels); the **adapters** that map each source's own vocabulary onto it are **open/pluggable** — matching "sources attributed dynamically, never hardcoded." Each first-class source (CodeRabbit first) ships a parser; everything else falls through to defaults (failing check → `MAJOR`; bare human/bot comment or unparseable → `minor`). New sources = new adapters, no ladder change. (Reversible; parked here, not an ADR.)

## Components

### Hub (Go + Gin + SQLite)
One portable service; identical artifact self-hosted or run as SaaS ([ADR-0001](./docs/adr/0001-portable-go-sqlite-hub-over-cloudflare.md)). HTTP layer is Gin (SSE routes exempted from buffering middleware).

- **Webhook ingress** — `POST /webhooks/github`, verifies `X-Hub-Signature-256`, dedupes, buckets by `owner/repo#number @ sha` (Round).
- **Round settle** — on `check_suite completed` + grace window, run the mergeability poll, compile the summary.
- **SSE** — `GET /sse/{owner}/{repo}/{number}` (authenticated via Hub-minted installation token — [ADR-0003](./docs/adr/0003-sse-auth-via-hub-minted-installation-token.md)); fans compiled summaries out to all held connections for the key.
- **Pending store** — SQLite; latest summary per PR per signal-type when no listener; newer same-type events replace, nothing else clears. `GET /pending` (one-shot startup check, returns all). No `ack` ([ADR-0006](./docs/adr/0006-pending-is-latest-state-per-type-consumer-owns-relevance.md)).
- **Rebase lease** — Hub grants the single force-push lease to one actor; for orphaned PRs the Hub itself takes it: "Update branch" + enable auto-merge ([ADR-0002](./docs/adr/0002-agent-owned-rebase.md), [ADR-0005](./docs/adr/0005-hub-granted-rebase-lease.md)).

### Watcher (MCP server)
Local, runs in the harness; the universal contract across harnesses.

- `subscribe_pr(owner, repo, number)` — opens & holds the SSE; returns compiled summaries as they arrive (fan-out: co-existing with other listeners is fine).
- `get_pending()` — one-shot; returns all current Pending items across the installation, each with `timestamp`, head `SHA`, and PR state for relevance filtering.
- `acquire_rebase_lease(owner, repo, number)` — requests the single force-push lease before rebasing; granted to one actor ([ADR-0005](./docs/adr/0005-hub-granted-rebase-lease.md)).
- Renders summaries (label + symbol + colour). SessionStart hook reminds the agent to call `get_pending()`.

### GitHub App
Auth for both modes via the App Manifest flow ([ADR rationale in CONTEXT](./CONTEXT.md)).

- **SaaS:** hosted App; user clicks *Install*, picks repos. No stored PATs.
- **Self-host:** Manifest flow provisions the user's own App pointing at their Hub.
- Events: `check_suite`, `pull_request`, `pull_request_review`, `pull_request_review_comment`, `issue_comment`.
- Permissions (pin exact scopes at build): `pull_requests:read/write`, `contents:write`, `checks:read`.
- The **local agent keeps its own `gh`/git creds** — the App is the Hub's identity only.

## Deploy modes

- **Self-host:** `docker run` the Hub, run the Manifest flow, point the Watcher at your Hub URL.
- **SaaS:** install the hosted App, install the Watcher, subscribe. (Pricing TBD.)

## Build slices (suggested order)

1. **Hub core** — webhook ingest + signature verify + Round bucketing + SQLite pending store.
2. **Compile + SSE** — `check_suite`+grace settle (+ same-SHA re-settle), summary compiler, fan-out SSE push, installation-token auth, `get_pending`.
3. **Mergeability poll** — settle-time poll; emit the Mergeability signal.
4. **Watcher MCP** — `subscribe_pr` / `get_pending` / `ack`, summary rendering, SessionStart hook.
5. **GitHub App + Manifest** — App, install/manifest flows, installation tokens.
6. **Rebase** — Session-side rebase guidance + Hub orphan fallback + auto-merge.
7. **Severity** — CodeRabbit parser → normalised severity; label/symbol/colour rendering.

## Open questions (parked for build time)

- Exact GitHub App permission scopes.
- CodeRabbit comment format → severity parsing (and the next first-class source).
- Grace-window duration (start ~30s; tune) and rebase-lease TTL (~60–120s; tune).
- Channels: re-add as an optional Claude push lane once it reaches general release.

_Resolved during design grilling: SSE auth ([ADR-0003](./docs/adr/0003-sse-auth-via-hub-minted-installation-token.md)); fan-out vs. single-owner ([ADR-0002](./docs/adr/0002-agent-owned-rebase.md) amend, [ADR-0005](./docs/adr/0005-hub-granted-rebase-lease.md)); re-settle ([ADR-0004](./docs/adr/0004-rounds-re-settle-on-late-same-sha-signals.md)); pending model ([ADR-0006](./docs/adr/0006-pending-is-latest-state-per-type-consumer-owns-relevance.md))._

## Status

Pre-implementation. Design converged; see `CONTEXT.md` + ADRs. No code yet.
