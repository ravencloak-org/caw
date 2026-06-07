# 0006 — Pending is a latest-state store per signal-type; the consumer owns relevance

**Status:** Accepted
**Date:** 2026-06-07

## Decision

The Pending store holds, per `owner/repo#number` **per signal-type**, the **latest** compiled summary for that type. A newer same-type event **replaces** the stored one (keyed by timestamp); the store is latest-state, not an append log and not a history. The Hub does **not** garbage-collect Pending items — nothing expires them by age, by `pull_request closed/merged`, or by head-SHA move. They persist until overwritten by a newer same-type event.

A starting Session's one-shot `get_pending()` returns **all** current items for its installation. Choosing which to act on, and how (serially, in parallel, or skip), is the **consumer's** responsibility, not the Hub's. To make that judgment cheap, every Pending item carries its `timestamp`, head `SHA`, and PR state (open/closed/merged).

There is no `ack`: because acting on a PR produces new GitHub events that overwrite the relevant same-type Pending item, an explicit acknowledgement has nothing to clear.

## Why

The Hub should not encode policy about what work matters — different harnesses and humans will triage differently. Keeping the Hub a dumb latest-state store keeps it simple and harness-agnostic, and pushes triage to the agent/human who actually has context. "Latest per type" gives a clean, bounded-per-PR snapshot (not a confusing stack of partial re-settles from ADR-0004) while still never dropping a distinct signal-type.

Returning the SHA and PR state with each item lets the consumer filter dead items (merged PR, stale SHA) without the Hub making that call — so "nothing clears" does not translate into startup noise.

## Trade-offs accepted

- **Unbounded store growth in principle:** abandoned PRs that never close and never get a newer event keep their items forever. Accepted deliberately; if it becomes a real operational problem, a TTL backstop can be added later without changing the contract (it would only drop items the consumer would already filter as stale).
- The consumer must do its own relevance filtering on every startup. Accepted — it is the only actor with the context to do it well, and the per-item metadata makes it cheap.

## Considered alternatives

- **Event-driven invalidation (delete on close/merge/new-SHA) + TTL:** rejected as the default — it bakes relevance policy into the Hub. Kept in reserve as a pure backstop if growth bites.
- **Append-log of every settle (full history):** rejected — surfaces redundant partial re-settles (ADR-0004) and grows far faster, for history no consumer asked for.
- **Explicit `ack` to clear items:** rejected as redundant — new GitHub events from acting already overwrite the item.
