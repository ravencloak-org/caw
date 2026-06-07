# 0001 — Portable Go + SQLite Hub over Cloudflare-native

**Status:** Accepted
**Date:** 2026-06-07

## Decision

The Hub is a single portable service — Go + embedded SQLite (optional Postgres at SaaS scale), plain SSE — packaged as one artifact that runs identically whether a user self-hosts it or we operate it as the SaaS. We are **not** building the Hub on Cloudflare Workers + Durable Objects + D1.

Within Go, the HTTP layer is the **Gin** framework (routing + middleware: signature verify, installation-token auth, logging, recovery). SSE is a manual streaming handler (`c.Stream` + flush + `Request.Context().Done()` for disconnect); SSE routes are exempted from any buffering/gzip middleware and send `X-Accel-Buffering: no`. Gin sits behind `http.Handler`, so it is reversible and not separately ADR'd.

## Why

Self-hosting is a first-class product option: a user must be able to `docker run` their own Hub and point their Watcher at it. Durable Objects and D1 are Cloudflare-proprietary and cannot be self-hosted, so a CF-native Hub would force a second, separate backend for self-hosters — two codebases, drifting features, double the bugs. One portable artifact keeps "self-host or subscribe" a deploy-time flag rather than two products, and matches existing Go expertise.

## Trade-offs accepted

- We give up Durable Objects' built-in per-entity connection-holding and durable state. Holding SSE connections with SQLite-backed state is a well-understood Go problem, so this is a modest cost.
- The SaaS runs the same image at scale (behind Cloudflare as CDN/tunnel, not as the app runtime), so we operate a stateful service rather than leaning on serverless autoscaling.

## Considered alternatives

- **Cloudflare Worker + DO + D1 (SaaS-only):** rejected — breaks first-class self-host.
- **Two backends (CF for SaaS, Go for self-host):** rejected — two codebases to maintain in lockstep.
- **Kotlin + Spring Boot:** rejected for *this* workload — JVM footprint/startup (or GraalVM native build friction) fights the lean self-host artifact; holding many SSE connections cheaply forces WebFlux (reactive), which then mismatches blocking JDBC over embedded SQLite. Go is a connection-holding network daemon natively. Spring's strengths (DI, JPA, enterprise patterns, big-team scale) are not what this Hub needs. Would revisit only if the Hub became business-logic-heavy or the team turned Kotlin-first with self-host demoted.
- **TypeScript on Bun:** viable on the artifact axis (`bun:sqlite`, native SSE, `bun build --compile` → single binary), but Go was chosen for existing expertise and the maturity of goroutine-per-connection SSE. OpenTUI was considered but is a terminal-UI renderer, not a backend — not applicable to the Hub.
