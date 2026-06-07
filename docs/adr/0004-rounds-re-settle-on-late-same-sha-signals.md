# 0004 — Rounds re-settle on late same-SHA signals (one summary per settle, not per Round)

**Status:** Accepted
**Date:** 2026-06-07

## Decision

A Round settles when the grace timer fires after the latest `check_suite` completion, and emits **one compiled summary per settle**. A late signal arriving on the **same head SHA** (an async review comment, a check that flipped state) **re-arms the grace timer and re-settles** the Round, emitting a fresh summary tagged with a monotonically increasing `seq`. A **new head SHA** opens a new Round and supersedes the prior Round's unacked items.

Re-settle is bounded by **SHA-currency and PR-open state, not a timer**. A late signal whose SHA is no longer the head is dropped (superseded); a late signal on a still-current SHA with no live listener becomes a Pending item.

## Why

We deliberately do **not** barrier on all three signal-types before settling — Squawk fires only on migration PRs and CodeRabbit is asynchronous, so waiting for "all signals" would hang indefinitely. But settling early means an async signal can arrive *after* the Round's summary was already pushed. "Exactly one summary per Round" and "settle before the async signals arrive" cannot both hold. Relaxing the invariant to **one summary per settle, with same-SHA re-settle** keeps the no-barrier decision and still never drops a late comment.

Bounding re-settle by SHA + open-state (rather than a tunable timer) reuses invariants the system already has: a new push supersedes, and a late signal with no listener falls to the Pending path (drained at next startup, gated by a human prompt). So a comment posted hours later cannot spuriously wake a fresh agent; it only reaches a still-live session, where the update is wanted.

## Trade-offs accepted

- An agent may receive **more than one summary for the same head SHA** (e.g. the CI summary, then a CodeRabbit summary ~a minute later). Acceptable: with fan-out and git's non-fast-forward arbitration (ADR-0002), an agent already handles each summary independently.
- Slightly more state per Round (last-settle `seq`, re-arm tracking) than a one-shot settle.

## Considered alternatives

- **Barrier on all signal-types before settling:** rejected — async/absent reviewers hang the Round.
- **One-shot settle, drop late same-SHA signals:** rejected — silently loses real review feedback.
- **Hard re-arm time cap (N minutes):** rejected as unnecessary — SHA-currency + PR-open + the Pending path already bound it without a tunable magic number.
