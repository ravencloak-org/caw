-- Dolt (MySQL-dialect) schema for caw.
-- This is the authoritative source; generate SQLite + Postgres via `make schema`.
-- Do NOT hand-edit internal/store/schema.sql or internal/store/schema.postgres.sql.

-- Deduplication of GitHub webhook deliveries by X-GitHub-Delivery id.
CREATE TABLE IF NOT EXISTS deliveries (
    id          VARCHAR(255) NOT NULL,
    event       TEXT         NOT NULL,
    received_at BIGINT       NOT NULL,
    PRIMARY KEY (id)
);

-- A Round is the lifecycle of a single PR head SHA (owner/repo#number @ sha).
-- A new head SHA is a new Round; see ADR-0004.
CREATE TABLE IF NOT EXISTS rounds (
    owner      VARCHAR(255) NOT NULL,
    repo       VARCHAR(255) NOT NULL,
    number     INT          NOT NULL,
    sha        VARCHAR(40)  NOT NULL,
    updated_at BIGINT       NOT NULL,
    PRIMARY KEY (owner, repo, number, sha)
);

-- Latest-state pending store: at most one row per PR per signal-type, the
-- newest summary wins (ADR-0006). Nothing here is garbage-collected; a newer
-- same-type event overwrites the row.
CREATE TABLE IF NOT EXISTS pending (
    owner       VARCHAR(255) NOT NULL,
    repo        VARCHAR(255) NOT NULL,
    number      INT          NOT NULL,
    signal_type VARCHAR(64)  NOT NULL,
    sha         VARCHAR(40)  NOT NULL,
    pr_state    VARCHAR(64)  NOT NULL,
    summary     TEXT         NOT NULL,
    updated_at  BIGINT       NOT NULL,
    PRIMARY KEY (owner, repo, number, signal_type)
);

-- Raw signals observed for a Round, compiled into a summary at settle time.
-- Keyed by their natural identity so a re-delivered signal replaces, not dupes.
CREATE TABLE IF NOT EXISTS signals (
    owner       VARCHAR(255) NOT NULL,
    repo        VARCHAR(255) NOT NULL,
    number      INT          NOT NULL,
    sha         VARCHAR(40)  NOT NULL,
    signal_type VARCHAR(64)  NOT NULL,           -- checks | comments | mergeability
    source      VARCHAR(255) NOT NULL,           -- dynamic attribution, e.g. CI / CodeRabbit
    external_id VARCHAR(255) NOT NULL,           -- per-source id for replace/dedupe
    severity    VARCHAR(32)  NOT NULL DEFAULT '',
    body        TEXT         NOT NULL DEFAULT '',
    updated_at  BIGINT       NOT NULL,
    PRIMARY KEY (owner, repo, number, sha, signal_type, source, external_id)
);

-- Hub-minted installation tokens for SSE / get_pending auth (ADR-0003).
-- Only the hash is stored; the raw token is shown once at mint time.
CREATE TABLE IF NOT EXISTS tokens (
    token_hash      VARCHAR(64)  NOT NULL,        -- hex sha256 of the raw token
    installation_id VARCHAR(255) NOT NULL,
    org             VARCHAR(255) NOT NULL DEFAULT '',
    created_at      BIGINT       NOT NULL,
    PRIMARY KEY (token_hash)
);

-- GitHub App installations. One row per installation_id.
CREATE TABLE IF NOT EXISTS installations (
    installation_id VARCHAR(255) NOT NULL,
    account_login   VARCHAR(255) NOT NULL,
    created_at      BIGINT       NOT NULL,
    PRIMARY KEY (installation_id)
);

-- Repos associated with each installation.
CREATE TABLE IF NOT EXISTS installation_repos (
    installation_id VARCHAR(255) NOT NULL,
    full_name       VARCHAR(512) NOT NULL,           -- "owner/repo"
    PRIMARY KEY (installation_id, full_name)
);

-- Rebase leases: at most one holder per PR at any time (ADR-0005).
-- UNIQUE(org, repo, pr_number) enforces the single-owner invariant.
-- All timestamps are Unix epoch seconds (BIGINT) to stay portable.
CREATE TABLE IF NOT EXISTS leases (
    org               VARCHAR(255) NOT NULL,
    repo              VARCHAR(255) NOT NULL,
    pr_number         INT          NOT NULL,
    holder            VARCHAR(255) NOT NULL,          -- installation_id of current holder
    expires_at        BIGINT       NOT NULL,          -- Unix epoch; grant duration set by Hub
    last_heartbeat_at BIGINT       NOT NULL,          -- updated by holder; orphan detection uses this
    acquired_at       BIGINT       NOT NULL,
    UNIQUE KEY uq_lease (org, repo, pr_number)
);

CREATE INDEX leases_expires ON leases (expires_at);

-- GitHub App credentials persisted after the manifest conversion flow.
-- Only one row ever exists (single-app deployment); enforced by CHECK (id = 1).
CREATE TABLE IF NOT EXISTS app_credentials (
    id             INT          NOT NULL CHECK (id = 1),
    app_id         VARCHAR(64)  NOT NULL,
    client_id      VARCHAR(64)  NOT NULL,
    client_secret  TEXT         NOT NULL,
    webhook_secret TEXT         NOT NULL,
    pem            TEXT         NOT NULL,
    created_at     BIGINT       NOT NULL,
    PRIMARY KEY (id)
);
