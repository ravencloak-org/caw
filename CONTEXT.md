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
The link between a Session and a PR, keyed `owner/repo#number` and bound to that Session's live SSE connection. Present = originator is listening; absent = orphaned.
_Avoid_: connection, channel.

**Round**:
One settle-cycle for a single PR head SHA (`owner/repo#number @ sha`). A new push opens a new Round and supersedes the prior Round's unacked items.
_Avoid_: run, cycle, batch.

**Signal-type**:
The three kinds of feedback Caw surfaces — **Checks**, **Comments**, **Mergeability**. Sources within each are attributed dynamically, never hardcoded.
_Avoid_: category, event type.

**Pending item**:
A compiled summary for a PR with no live Subscription, stored by the Hub until a Session retrieves it via the one-shot startup check.
_Avoid_: queued event, backlog.

## Relationships

- A **Session** opens one **Subscription** per PR it raises; the **Subscription** owns one held-open SSE connection.
- The **Hub** ingests GitHub webhooks for a **Round** and emits exactly one compiled summary per Round per PR.
- A summary carries one or more **Signal-types**; **Comments** and **Checks** attribute their **source** dynamically.
- **Mergeability** is the only signal produced by a poll (after a Round settles), never by a webhook.
- No live **Subscription** → the summary becomes a **Pending item**; the next **Session** drains it via a single startup request.

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
