# db/ — Dolt schema repository

This directory is a [Dolt](https://docs.dolthub.com/) database that serves as
the **source of truth for caw's schema**. It is a build/dev-time dependency
only — the Hub runtime does not access it.

Dolt is MySQL-dialect with Git-like versioning (branch, commit, diff, merge).

## Prerequisites

Install Dolt (macOS):

```sh
brew install dolt
```

Or download from https://github.com/dolthub/dolt/releases.

### Pinned version

`dolt schema export` formatting can differ between releases, which would make
the generated dialect files non-reproducible. The version is therefore pinned
to **2.1.2** in `.github/workflows/sql.yml` (`DOLT_VERSION`). The committed
files in `internal/store/` were generated with this version.

If you regenerate with a different version and CI's drift gate fails, either
install dolt 2.1.2 locally or bump `DOLT_VERSION` in the workflow **and**
regenerate + commit with that same version. Keep local and CI versions
identical.

## Regenerate dialect files

After any schema change in Dolt, regenerate the SQLite and Postgres dialect
files:

```sh
make schema
```

This runs `scripts/gen-schema.sh`, which calls `dolt schema export` and pipes
the output through `scripts/translate-schema.py`. The two generated files are:

- `internal/store/schema.sql` — SQLite dialect, loaded by the Hub runtime
- `internal/store/schema.postgres.sql` — PostgreSQL dialect, for future use

Both files are committed to git. **Do not edit them by hand.**

## Making a schema change

```sh
# 1. Navigate into the Dolt repo
cd db

# 2. Start a Dolt SQL session and alter the schema
dolt sql -q "ALTER TABLE leases ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;"

# 3. Review the diff
dolt diff

# 4. Commit the change in Dolt
dolt add .
dolt commit -m "feat: add retry_count to leases"

# 5. Go back to the repo root and regenerate
cd ..
make schema

# 6. Run tests
go test -race ./...

# 7. Commit both Dolt changes and generated dialect files together
git add db/ internal/store/schema.sql internal/store/schema.postgres.sql
git commit -m "feat(schema): add retry_count to leases"
```

## Viewing schema history

```sh
cd db
dolt log
dolt diff HEAD~1 HEAD
```
