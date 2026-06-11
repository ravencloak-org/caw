# ADR-0009 — Dolt as versioned schema source-of-truth

| Field       | Value                              |
|-------------|-------------------------------------|
| Status      | Accepted                            |
| Date        | 2026-06-10                          |
| Amends      | ADR-0001 (schema storage location)  |

## Context

ADR-0001 established `internal/store/schema.sql` (SQLite) as the Hub's schema
definition. As the project grows, two pain points have emerged:

1. **No schema history.** Git diffs on raw SQL are noisy and context-free.
   There is no way to diff schema state between commits without reading SQL
   manually.

2. **No multi-dialect path.** The Hub currently targets SQLite
   (`modernc.org/sqlite`). A future deployment target (e.g. Postgres on
   Supabase/RDS) would require a separate, hand-maintained dialect file with
   no guarantee of parity.

## Decision

We adopt [Dolt](https://docs.dolthub.com/) as a **build/dev-time** schema
VCS, stored in `db/`. Dolt is a MySQL-dialect versioned database with Git-like
branching, commits, and diffs. It is **not** a runtime dependency of the Hub.

A generator (`scripts/gen-schema.sh` + `scripts/translate-schema.py`) reads
the schema from Dolt via `dolt schema export` and emits two dialect-specific
files:

| File | Dialect | Consumer |
|------|---------|----------|
| `internal/store/schema.sql` | SQLite | Hub runtime (`modernc.org/sqlite`) |
| `internal/store/schema.postgres.sql` | PostgreSQL | Future use / reference |

Both files are committed to git and must never be edited by hand.

A CI drift gate in `.github/workflows/sql.yml` fails the build if the
committed dialect files diverge from `make schema` output.

## Workflow

```
# Make a schema change
cd db
dolt sql -q "ALTER TABLE leases ADD COLUMN ..."
dolt add .
dolt commit -m "feat: add column X to leases"

# Regenerate dialect files
make schema          # rewrites internal/store/schema.sql + schema.postgres.sql

# Verify tests still pass
go test -race ./...

# Commit both the Dolt change and the generated dialect files together
git add db/ internal/store/schema.sql internal/store/schema.postgres.sql
git commit -m "feat(schema): ..."
```

## Rationale

- Dolt provides column-level diff, branch isolation for speculative changes,
  and a queryable schema history — all without requiring a running server.
- The generator is ~200 lines of pure Python with no external dependencies
  (no ORM, no SQL parser library). It handles the MySQL→SQLite and
  MySQL→Postgres type mappings explicitly, making the translation transparent.
- SQLite type affinity rules mean `TEXT PRIMARY KEY` and `TEXT NOT NULL, PRIMARY KEY (id)`
  are semantically identical. The generator uses the explicit form for clarity.
- The Postgres file uses idiomatic types (TEXT over varchar(n), named UNIQUE
  constraints) per Postgres community conventions.
- The drift gate ensures `db/` and the generated files are always in sync,
  preventing the "forgot to regenerate" failure mode.

## Consequences

- **Dolt** (`/opt/homebrew/bin/dolt` on macOS, or `dolt` in CI) must be
  installed on developer machines and in the schema-drift CI job.
- `internal/store/schema.sql` is now a **generated file**. It retains its
  role as the runtime schema loaded by `modernc.org/sqlite` and must not be
  edited directly.
- `internal/store/schema.postgres.sql` is a new file, not yet consumed by
  any runtime code.
- The Dolt repo at `db/` is a nested VCS directory. It is **not** a git
  submodule; its `.dolt/` directory is committed as ordinary files.
- Schema changes now follow a two-step commit pattern: (1) commit to Dolt,
  (2) regenerate and commit to git.
