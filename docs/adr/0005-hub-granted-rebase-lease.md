# 0005 — Hub-granted rebase lease (single grant point for listeners and the orphan fallback)

**Status:** Accepted
**Date:** 2026-06-07

## Decision

The right to force-push a rebased branch is an explicit **lease granted by the Hub**, never assumed by an actor. Any actor intending to rebase — a listening Session, or the Hub itself for an orphan — calls one grant point that hands the lease to **exactly one** holder via the `UNIQUE(org_id, repo_name, pr_number)` row. Losers stand down and wait for the next Round (the winner's force-push opens a new head SHA → new Round → everyone re-syncs). The lease carries a **TTL with heartbeat** (~60–120s, renewed while rebasing); on expiry the Hub reclaims it and re-grants or performs the orphan rebase itself.

## Why

Fan-out (ADR-0002 amendment) means *N* listeners can receive the same "behind base → rebase" mergeability signal simultaneously. Without an explicit grant they would all force-push and trample each other — the exact failure ADR-0002 exists to prevent, re-introduced *between listeners*. Making the lease Hub-granted reduces "who rebases?" to a single atomic decision.

Routing the Hub's orphan fallback through the **same** grant point closes the original Hub-vs-Session race too: the "zero listeners → Hub rebases" decision and a late-arriving subscriber that also wants to rebase both contend for one lease, so they can never both force-push.

The `UNIQUE(org, repo, pr)` constraint — explicitly *not* the right tool for subscription exclusivity (subscriptions fan out) — is exactly right here: the rebase lease is the one genuinely single-owner resource in the system.

A TTL+heartbeat is the **only** timer we keep, because a crashed lease-holder mid-rebase cannot be detected by SHA-currency or PR-open state — a hung actor needs a deadline.

## Trade-offs accepted

- An extra round-trip (`acquire_rebase_lease`) before a rebase. Negligible: rebase is already a heavyweight, infrequent operation.
- Lease bookkeeping (holder, TTL, heartbeat, reclaim) is real state the Hub must manage.

## Considered alternatives

- **Let everyone rebase, rely on `git push --force-with-lease`:** rejected. It prevents *silent* overwrite (the second push fails if the ref moved), but it does not prevent wasteful concurrent rebases, leaves messy per-actor failure handling, and — critically — cannot coordinate the **Hub-vs-Session** orphan race, since they are different actors with independent local state. Hub-granted leasing solves all three.
- **Listener self-elects (e.g. lowest session ID rebases):** rejected — requires every listener to know the full listener set and agree, which the fan-out model does not give them; the Hub already knows.
