# 0002 — Agent-owned rebase, Hub rebases only orphans

**Status:** Accepted
**Date:** 2026-06-07
**Amended:** 2026-06-07 — subscriptions now fan out (many listeners per PR); the single-owner rule below applies **only to the rebase / force-push**, expressed as a per-PR **Rebase lease**. Ordinary fixes from concurrent listeners are arbitrated by git's non-fast-forward rejection and need no lease. See CONTEXT.md → *Rebase lease*.

## Decision

When a PR is mergeable-but-behind-base with no conflicts, the **listening Session** performs the rebase in its own worktree and pushes. The **Hub** performs a server-side rebase only as a fallback for **orphaned** PRs (no live Subscription). Auto-merge (`--auto --squash`) is set by whichever actor acted.

## Why

A Hub force-pushing a rebased branch can race with work the local agent or human is doing on that same branch head — a real, previously-experienced failure mode (commits lost to branch trampling when the working directory is switched out from under an actor). The listening Session is the only actor that knows whether it is mid-edit and owns its worktree, so it is the safe place to rebase. Orphaned PRs have no local actor to trample, so the Hub can rebase them without risk.

## Trade-offs accepted

- The Hub needs `contents:write` on the GitHub App for the orphan fallback path even though it rarely uses it.
- Rebase behaviour differs by whether a Session is listening — slightly more logic than "Hub always rebases," accepted to eliminate the trampling race.

## Considered alternatives

- **Hub always rebases server-side:** rejected — force-push races with live local work; reintroduces the trampling failure.
- **Never auto-rebase, always report only:** rejected — loses the hands-off value for orphaned PRs that just need a clean rebase.
