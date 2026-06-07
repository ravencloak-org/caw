# 0007 — Subscriptions fan out; the PR key is the only router

**Status:** Accepted
**Date:** 2026-06-07

## Decision

Any number of Sessions may hold a Subscription to the same PR. The Hub pushes each compiled summary to **all** live connections for `owner/repo#number` (fan-out). Routing is by the **PR key alone** — at most one logical stream per key, delivered to every listener on it. There is no "originator" identity enforced at the Hub: typically the Session that raised the PR is among the listeners, but that is neither proven nor required.

The only single-owner resource in the system is the **rebase lease** (ADR-0005). Ordinary concurrent pushes need no coordination — git's non-fast-forward rejection arbitrates them.

## Why

An MCP Watcher has no durable, harness-portable identity the Hub can verify, and the Hub only ever learns of a PR through GitHub webhooks (which carry the GitHub author, never a harness session id). So "deliver only to the agent that raised the PR" cannot be *enforced* without inventing a credential. Rather than build that machinery for a guarantee that buys nothing operationally, we fan out: the PR key is already a unique router, and in practice the raiser is the listener.

Fan-out also collapses three other problems:
- **SSE auth** drops from "prove exclusive ownership of this PR" to a repo-scoped visibility check (ADR-0003).
- The `UNIQUE(org, repo, pr)` constraint is freed from subscriptions and put where it belongs — the rebase lease.
- **Concurrency** is safe by default: git rejects non-fast-forward pushes, so a losing concurrent fixer simply re-pulls; only force-push (rebase) needs the lease.

## Trade-offs accepted

- Two Sessions deliberately pointed at one PR will both receive every summary and may both attempt fixes — wasted effort, but no corruption (git arbitrates; rebase is leased). This is a user choice, not a failure mode the Hub prevents.
- "The agent that raised the PR" becomes the *typical* case, not a *guaranteed* one — the product story is honest about this (CONTEXT.md → Flagged ambiguities).

## Considered alternatives

- **Single-owner subscription keyed by a session id:** rejected — a self-asserted session id is a name, not a credential, and is harness-specific (breaks harness-agnostic); it adds nothing to routing the PR key doesn't already give.
- **Single-owner subscription with a real per-PR token:** rejected — exclusivity solves a problem fan-out doesn't have, at the cost of reconnect/handoff complexity.
