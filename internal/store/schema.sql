-- Deduplication of GitHub webhook deliveries by X-GitHub-Delivery id.
CREATE TABLE IF NOT EXISTS deliveries (
    id          TEXT PRIMARY KEY,
    event       TEXT NOT NULL,
    received_at INTEGER NOT NULL
);

-- A Round is the lifecycle of a single PR head SHA (owner/repo#number @ sha).
-- A new head SHA is a new Round; see ADR-0004.
CREATE TABLE IF NOT EXISTS rounds (
    owner      TEXT    NOT NULL,
    repo       TEXT    NOT NULL,
    number     INTEGER NOT NULL,
    sha        TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, number, sha)
);

-- Latest-state pending store: at most one row per PR per signal-type, the
-- newest summary wins (ADR-0006). Nothing here is garbage-collected; a newer
-- same-type event overwrites the row.
CREATE TABLE IF NOT EXISTS pending (
    owner       TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    number      INTEGER NOT NULL,
    signal_type TEXT    NOT NULL,
    sha         TEXT    NOT NULL,
    pr_state    TEXT    NOT NULL,
    summary     TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, number, signal_type)
);

-- Raw signals observed for a Round, compiled into a summary at settle time.
-- Keyed by their natural identity so a re-delivered signal replaces, not dupes.
CREATE TABLE IF NOT EXISTS signals (
    owner       TEXT    NOT NULL,
    repo        TEXT    NOT NULL,
    number      INTEGER NOT NULL,
    sha         TEXT    NOT NULL,
    signal_type TEXT    NOT NULL,           -- checks | comments | mergeability
    source      TEXT    NOT NULL,           -- dynamic attribution, e.g. CI / CodeRabbit
    external_id TEXT    NOT NULL,           -- per-source id for replace/dedupe
    severity    TEXT    NOT NULL DEFAULT '',
    body        TEXT    NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (owner, repo, number, sha, signal_type, source, external_id)
);

-- Hub-minted installation tokens for SSE / get_pending auth (ADR-0003).
-- Only the hash is stored; the raw token is shown once at mint time.
CREATE TABLE IF NOT EXISTS tokens (
    token_hash      TEXT PRIMARY KEY,        -- hex sha256 of the raw token
    installation_id TEXT NOT NULL,
    org             TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);

-- GitHub App installations. One row per installation_id.
CREATE TABLE IF NOT EXISTS installations (
    installation_id TEXT PRIMARY KEY,
    account_login   TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);

-- Repos associated with each installation.
CREATE TABLE IF NOT EXISTS installation_repos (
    installation_id TEXT NOT NULL,
    full_name       TEXT NOT NULL,           -- "owner/repo"
    PRIMARY KEY (installation_id, full_name)
);

-- Rebase leases: at most one holder per PR at any time (ADR-0005).
-- UNIQUE(org, repo, pr_number) enforces the single-owner invariant.
-- All timestamps are Unix epoch seconds (INTEGER) to stay portable to Postgres.
CREATE TABLE IF NOT EXISTS leases (
    org               TEXT    NOT NULL,
    repo              TEXT    NOT NULL,
    pr_number         INTEGER NOT NULL,
    holder            TEXT    NOT NULL,          -- installation_id of current holder
    expires_at        INTEGER NOT NULL,          -- Unix epoch; grant duration set by Hub
    last_heartbeat_at INTEGER NOT NULL,          -- updated by holder; orphan detection uses this
    acquired_at       INTEGER NOT NULL,
    UNIQUE (org, repo, pr_number)
);

CREATE INDEX IF NOT EXISTS leases_expires ON leases (expires_at);

-- GitHub App credentials persisted after the manifest conversion flow.
-- Only one row ever exists (single-app deployment); enforced by CHECK (id = 1).
CREATE TABLE IF NOT EXISTS app_credentials (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    app_id         TEXT NOT NULL,
    client_id      TEXT NOT NULL,
    client_secret  TEXT NOT NULL,
    webhook_secret TEXT NOT NULL,
    pem            TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);
