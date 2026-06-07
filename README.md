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

**Live path.** A **Session** raises a PR; its **Watcher** (a local MCP server) opens and *holds* an SSE connection to the **Hub** keyed `owner/repo#number`. The Hub ingests that **Round**'s GitHub webhooks; once checks settle (`check_suite completed` + ~30s grace) it runs the one poll (mergeability), compiles **one** summary, and pushes it down the held connection. The Session acts in the PR's worktree, pushes, and the next Round begins. A new push supersedes the prior Round's unacked items (state is keyed by head SHA).

**Catch-up path.** No live Subscription (Session closed) → the summary is stored as a **Pending item**. A brand-new Session makes **one** request at startup ("anything pending?"), surfaced by a SessionStart hook, and prompts the human before acting on work it didn't start.

**The only poll in the whole system** is the mergeability/conflict re-verify after a Round settles. Everything else is webhook push (GitHub → Hub) and SSE push (Hub → Watcher). The startup pending-check is a single one-shot request, not polling.

## Signal-types

Feedback is surfaced as three dynamic signal-types; sources within each are attributed at runtime, never hardcoded.

| Signal-type | GitHub source | Notes |
|---|---|---|
| **Checks** | `check_suite` failures | CI, Squawk-as-check, Sonar-as-check… |
| **Comments** | `pull_request_review`, `pull_request_review_comment`, `issue_comment` | Any bot or human; severity normalised for first-class sources (CodeRabbit first) |
| **Mergeability** | poll of `GET /pulls/{n}` after settle | Behind-base & clean → Session rebases; orphan → Hub rebases; then auto-merge |

Severity renders as **label + symbol + colour** (e.g. `■ CRITICAL` / `▲ MAJOR` / `· minor`), never colour alone — readable on colourblind-accessible terminal themes and on no-colour terminals.

## Components

### Hub (Go + SQLite)
One portable service; identical artifact self-hosted or run as SaaS ([ADR-0001](./docs/adr/0001-portable-go-sqlite-hub-over-cloudflare.md)).

- **Webhook ingress** — `POST /webhooks/github`, verifies `X-Hub-Signature-256`, dedupes, buckets by `owner/repo#number @ sha` (Round).
- **Round settle** — on `check_suite completed` + grace window, run the mergeability poll, compile the summary.
- **SSE** — `GET /sse/{owner}/{repo}/{number}` (authenticated); pushes compiled summaries to a held connection.
- **Pending store** — SQLite; summaries with no live subscriber persisted until acked. `GET /pending` (one-shot startup check), `POST /ack`.
- **Rebase fallback** — for orphaned PRs only: "Update branch" + enable auto-merge ([ADR-0002](./docs/adr/0002-agent-owned-rebase.md)).

### Watcher (MCP server)
Local, runs in the harness; the universal contract across harnesses.

- `subscribe_pr(owner, repo, number)` — opens & holds the SSE for the Round; returns compiled summaries as they arrive.
- `get_pending()` — one-shot; returns unacked Pending items across the user's PRs.
- `ack(key)` — marks a summary handled.
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
2. **Compile + SSE** — `check_suite`+grace settle, summary compiler, SSE push, `get_pending`/`ack`.
3. **Mergeability poll** — settle-time poll; emit the Mergeability signal.
4. **Watcher MCP** — `subscribe_pr` / `get_pending` / `ack`, summary rendering, SessionStart hook.
5. **GitHub App + Manifest** — App, install/manifest flows, installation tokens.
6. **Rebase** — Session-side rebase guidance + Hub orphan fallback + auto-merge.
7. **Severity** — CodeRabbit parser → normalised severity; label/symbol/colour rendering.

## Open questions (parked for build time)

- Exact GitHub App permission scopes.
- CodeRabbit comment format → severity parsing (and the next first-class source).
- SSE auth: how the Watcher proves it owns `owner/repo#number` (tie to App installation / a per-Session token).
- Grace-window duration (start ~30s; tune).
- Channels: re-add as an optional Claude push lane once it reaches general release.

## Status

Pre-implementation. Design converged; see `CONTEXT.md` + ADRs. No code yet.
