#!/usr/bin/env bash
# gen-schema.sh — Generate SQLite and Postgres DDL from the Dolt schema source.
#
# Usage:
#   scripts/gen-schema.sh
#
# Outputs:
#   internal/store/schema.sql           — SQLite dialect (runtime, loaded by Hub)
#   internal/store/postgres/schema.sql  — Postgres dialect (future use)
#
# Dolt is a BUILD-TIME dependency only. It is NOT required to run the Hub.
# The generated files are committed; CI enforces that they match the Dolt source.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DB_DIR="$REPO_ROOT/db"
DOLT="${DOLT_BIN:-dolt}"

SQLITE_OUT="$REPO_ROOT/internal/store/schema.sql"
POSTGRES_OUT="$REPO_ROOT/internal/store/postgres/schema.sql"

# Export raw CREATE TABLE statements from Dolt
RAW_SCHEMA="$(cd "$DB_DIR" && "$DOLT" schema export 2>/dev/null)"

# Use Python for reliable multi-line regex translation
python3 "$SCRIPT_DIR/translate-schema.py" \
    --sqlite  "$SQLITE_OUT"   \
    --postgres "$POSTGRES_OUT" \
    <<< "$RAW_SCHEMA"

echo "Generated:"
echo "  $SQLITE_OUT"
echo "  $POSTGRES_OUT"
