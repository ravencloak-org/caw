# 0008 — Observability via OpenTelemetry; bundled OpenObserve as the default self-host sink

**Status:** Accepted
**Date:** 2026-06-07

## Decision

The Hub is instrumented with the **OpenTelemetry Go SDK** and emits traces, logs, and metrics over **OTLP** to a configurable endpoint (`OTEL_EXPORTER_OTLP_ENDPOINT`). The Hub is **backend-agnostic** — it stores no telemetry in its own database.

The **default self-host deployment bundles [OpenObserve](https://openobserve.ai)** as the OTLP sink: a single binary that, in single-node mode, uses **SQLite metadata + local disk** (no PostgreSQL, NATS, or etcd — those are cluster/HA only), ingests all three signals on one native OTLP endpoint, and ships **built-in visualization** (log search, metrics, trace waterfalls). Users may instead point the Hub's OTLP exporter at their own OpenObserve, Grafana/Tempo, Datadog, etc., or disable export entirely. At SaaS scale OpenObserve can move stream data to object storage (S3/GCS) and to Postgres metadata in cluster mode.

No Prometheus, ClickHouse, Elasticsearch, or Grafana is required.

## Why

Self-host is first-class (ADR-0001), so observability must add minimal overhead. OpenObserve collapses the 4–6-component LGTM stack into one lightweight binary with embedded SQLite + local disk — the same "lean single artifact, scale to Postgres/object-store only when needed" shape as the Hub itself, so it is philosophically consistent and doesn't reintroduce a heavy backend.

Storing OTel in the Hub's own Postgres/SQLite (the original idea) was rejected: there is **no maintained OTel→Postgres exporter** (Promscale, the old answer, was sunset in 2023; the Collector's only SQL-family exporter is ClickHouse), so we would own a custom ingestion path **and** have to build a trace-waterfall UI from scratch. OpenObserve gives storage *and* the viewer for free, at lower total overhead, while keeping the Hub vendor-neutral through plain OTLP.

Keeping the Hub OTLP-agnostic means "see your own OTel stack" is literal: whatever the user runs, they point the exporter at it.

## Trade-offs accepted

- The default self-host deployment runs **one extra process** (OpenObserve). Accepted: it is a single lightweight binary, far below Elastic/ClickHouse/LGTM overhead, and is optional/disable-able for a truly zero-sidecar run.
- Telemetry retention/compression is governed by **OpenObserve**, not the Hub.
- OpenObserve's *cluster/HA* mode needs PostgreSQL + NATS — but single-node self-host does not, so the SQLite-default promise (ADR-0001) holds for the common case.

## Considered alternatives

- **Store OTel in the Hub's Postgres/TimescaleDB:** rejected — no turnkey exporter post-Promscale; forces us to build a trace UI; would pressure Postgres-as-mandatory for self-host. Timescale's storage primitives are good but the ingestion+view layer is the missing, expensive part.
- **Emit OTLP only, bring-your-own backend:** kept as the escape hatch, not the default — a zero-infra self-hoster would otherwise see nothing out of the box.
- **Grafana LGTM (Loki/Tempo/Mimir/Grafana) or Prometheus+Tempo+Loki:** rejected — 4–6 components, heavy for self-host.
- **ClickHouse-based (SigNoz / native ClickHouse exporter):** rejected — reintroduces the columnar-OLAP backend overhead we set out to avoid.
