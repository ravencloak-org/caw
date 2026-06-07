# Caw

Caw pushes PR feedback to the agent that raised the PR, so the agent fixes its own PR without being prodded. GitHub webhooks (failing checks, review comments, mergeability) are compiled into one summary and delivered over a held-open SSE connection to the listening agent — or held as pending for the next agent to pick up. Harness-agnostic (any MCP client) and reviewer-agnostic (any commenting bot or human).

## Language

**Session**:
The agent that raised the PR — Claude, Gemini, OpenAI, or any MCP-capable harness. The SSE client.
_Avoid_: agent (overloaded), cloud agent, client.

**Hub**:
The backend that receives GitHub webhooks, compiles them, persists state, serves SSE, and runs the one poll. A portable Go + SQLite service — the same artifact whether self-hosted or run as the SaaS.
_Avoid_: server, backend, API.

**Watcher**:
The local MCP server running inside the harness. Holds the SSE connection to the Hub and renders summaries (label + symbol + colour) into the Session.
_Avoid_: plugin, client, agent.

**Subscription**:
A live SSE connection from a Session to the Hub for one PR, keyed `owner/repo#number`. **Multiple Sessions may subscribe to the same PR** — every listener receives the same compiled summary (fan-out). At least one live Subscription = the PR is being watched; **zero live Subscriptions = orphaned**. Ordinary fixes from concurrent listeners need no coordination — git rejects non-fast-forward pushes, so the loser re-pulls. Only the **Rebase lease** is single-owner.
_Avoid_: connection, channel; "originator" as an enforced identity (typically the originator is among the listeners, but it is not proven or singular).

**Rebase lease**:
The right to force-push a rebased branch for a PR — held by **at most one actor**. A listening Session holds it for its own PR; the Hub holds it only as the orphan fallback. Ordinary commits need no lease (git's non-fast-forward guard is enough); rebases do, because force-push overwrites history with no git-level guard. This is the one place the single-owner rule survives fan-out (ADR-0002). The lease is **granted by the Hub** through one path for both listeners and the orphan fallback, and carries a TTL+heartbeat so a crashed holder is reclaimed ([[adr-0005]]).
_Avoid_: lock, mutex.

**Round**:
The lifecycle of a single PR head SHA (`owner/repo#number @ sha`). A Round **settles** when the grace timer fires after the latest `check_suite` completion, emitting **one compiled summary per settle**. A late signal on the **same SHA** (an async review comment, a check that flipped) **re-settles** the Round and emits a fresh summary with a monotonic `seq` — the invariant is *one summary per settle*, not one per Round. A **new head SHA** opens a new Round and supersedes the prior Round's unacked items. Re-settle is bounded by SHA-currency and PR-open state (not a timer); a late signal with no live listener becomes a **Pending item**. See [[adr-0004]].
_Avoid_: run, cycle, batch.

**Signal-type**:
The three kinds of feedback Caw surfaces — **Checks**, **Comments**, **Mergeability**. Sources within each are attributed dynamically, never hardcoded.
_Avoid_: category, event type.

**Pending item**:
The Hub's stored copy of the **latest** compiled summary **per `owner/repo#number` per signal-type**, kept whenever there is no live Subscription to push to. A newer event of the same signal-type **replaces** the stored one by timestamp — latest-state, not an append log. Pending items are **not** garbage-collected: nothing expires them by age, close, or merge; they persist until a newer same-type event overwrites them. A starting Session's one-shot `get_pending()` returns **all** current items across its installation; deciding which to act on — and how (serially, in parallel, or skip) — is the consumer's call, not the Hub's. Each item carries its `timestamp`, head `SHA`, and PR state so the consumer can judge relevance cheaply. See [[adr-0006]].
_Avoid_: queued event, backlog.

## Relationships

- A **Session** opens a **Subscription** per PR it cares about; many Sessions may hold a Subscription to the same PR, and all receive the same summary (fan-out).
- The **Hub** ingests GitHub webhooks for a **Round** and emits exactly one compiled summary per Round per PR.
- A summary carries one or more **Signal-types**; **Comments** and **Checks** attribute their **source** dynamically.
- **Mergeability** is the only signal produced by a poll (after a Round settles), never by a webhook.
- **Zero** live **Subscriptions** → the summary becomes a **Pending item**; the next **Session** retrieves all of them via a single startup request. A newer same-type event replaces a Pending item; nothing else clears it.

## Example dialogue

> **Dev:** "When CI fails on a PR the **Session** raised, does Caw poll GitHub?"
> **Domain expert:** "No. The **Watcher** is holding an SSE to the **Hub**; the **Hub** got the failure as a webhook and pushes the compiled summary down that connection. The *only* poll in the whole system is the **Mergeability** re-verify, once a **Round** settles."

> **Dev:** "What if the **Session** already closed?"
> **Domain expert:** "Then there's no live **Subscription**, so the summary is stored as a **Pending item**. The next agent that starts makes one request — 'anything pending?' — sees it, and asks the human before working on it. That single request isn't polling; it happens once at startup."

> **Dev:** "A PR is behind base but has no conflicts — who rebases?"
> **Domain expert:** "If a **Session** is listening, the **Session** rebases in its own worktree. If it's orphaned, the **Hub** rebases as a fallback. Either way auto-merge is set after."

## Flagged ambiguities

- "agent" was used to mean the Session, the Watcher, and the Hub all at once — resolved into three distinct terms.
- "cloud" / "cloud agent" meant the Anthropic Claude Agent session — resolved to **Session**.
- "subscribe to the agents" implied the Hub subscribes to Sessions — resolved: **Sessions subscribe to the Hub**; direction is always GitHub → Hub → SSE → Watcher → Session.
- "polling" was used for three different things — resolved: the *only* poll is the Mergeability re-verify; live delivery is SSE push; the startup pending-check is a single one-shot request, not polling.
- Channels (Claude Code research-preview push) — deferred; not part of v1. Will return as an optional per-harness push lane once it reaches general release. The portable contract is MCP either way.
- "originator" via a per-session ID was considered as the ownership key and rejected — a self-asserted session ID is a name, not a credential, and is Claude-specific (breaks harness-agnostic). Resolved: **listeners fan out**, routing is by the PR key `owner/repo#number`, and the only single-owner constraint is the **Rebase lease**. A session ID, where a harness exposes one, is at most a reconnect/continuity hint — never the routing key or the auth boundary.
