# 0001 — Portable Go + SQLite Hub over Cloudflare-native

**Status:** Accepted
**Date:** 2026-06-07

## Decision

The Hub is a single portable service — Go + embedded SQLite (optional Postgres at SaaS scale), plain SSE — packaged as one artifact that runs identically whether a user self-hosts it or we operate it as the SaaS. We are **not** building the Hub on Cloudflare Workers + Durable Objects + D1.

## Why

Self-hosting is a first-class product option: a user must be able to `docker run` their own Hub and point their Watcher at it. Durable Objects and D1 are Cloudflare-proprietary and cannot be self-hosted, so a CF-native Hub would force a second, separate backend for self-hosters — two codebases, drifting features, double the bugs. One portable artifact keeps "self-host or subscribe" a deploy-time flag rather than two products, and matches existing Go expertise.

## Trade-offs accepted

- We give up Durable Objects' built-in per-entity connection-holding and durable state. Holding SSE connections with SQLite-backed state is a well-understood Go problem, so this is a modest cost.
- The SaaS runs the same image at scale (behind Cloudflare as CDN/tunnel, not as the app runtime), so we operate a stateful service rather than leaning on serverless autoscaling.

## Considered alternatives

- **Cloudflare Worker + DO + D1 (SaaS-only):** rejected — breaks first-class self-host.
- **Two backends (CF for SaaS, Go for self-host):** rejected — two codebases to maintain in lockstep.
