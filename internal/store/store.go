// Package store is the Hub's SQLite-backed persistence layer.
//
// It uses modernc.org/sqlite — a pure-Go (cgo-free) driver — so the Hub
// compiles to a single static binary (ADR-0001).
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

//go:embed schema.sql
var schema string

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the schema.
//
// Schema bootstrap: schema.sql is `CREATE TABLE IF NOT EXISTS …` so it is a
// no-op on tables that already exist. To migrate v0.1.x databases online (which
// pre-date the Auth v2 token columns and the auth_sessions / installations.app_slug
// additions) Open also runs the additive migrations slice — each statement is
// idempotent because we swallow only the "duplicate column name" error SQLite
// raises when the column was already added. No data is touched.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Migrations FIRST: a v0.1.x DB has the legacy tokens / installations
	// tables but lacks the Auth v2 columns. schema.sql's CREATE INDEX
	// statements reference those columns, so they'd fail if applied first.
	// On a fresh DB the tables don't exist yet — applyMigrations swallows
	// the "no such table" error so the bootstrap path keeps working.
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrations contains additive schema statements that schema.sql's
// `CREATE TABLE IF NOT EXISTS` cannot apply to a pre-existing table. They are
// run BEFORE schema.sql on every Open (see Open's preamble for why);
// "duplicate column name" AND "no such table" are both treated as success so
// the call is idempotent across re-boots and safe on a fresh-bootstrap DB.
var migrations = []string{
	`ALTER TABLE tokens ADD COLUMN id                TEXT    NOT NULL DEFAULT ''`,
	`ALTER TABLE tokens ADD COLUMN github_user_id    INTEGER`,
	`ALTER TABLE tokens ADD COLUMN github_user_login TEXT`,
	`ALTER TABLE tokens ADD COLUMN device_label      TEXT    NOT NULL DEFAULT 'legacy'`,
	`ALTER TABLE tokens ADD COLUMN expires_at        INTEGER`,
	`ALTER TABLE tokens ADD COLUMN last_used_at      INTEGER`,
	`ALTER TABLE tokens ADD COLUMN revoked_at        INTEGER`,
	`ALTER TABLE installations ADD COLUMN app_slug   TEXT    NOT NULL DEFAULT ''`,
}

// applyMigrations runs each entry in migrations, swallowing the SQLite
// "duplicate column name" error so re-applying is a no-op.
func applyMigrations(db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			msg := err.Error()
			if strings.Contains(msg, "duplicate column name") ||
				strings.Contains(msg, "no such table") {
				continue
			}
			return fmt.Errorf("migration %q: %w", stmt, err)
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// HasDelivery reports whether a delivery id has already been recorded. Used as
// the pre-ingest dedupe check, so a delivery is only suppressed once it has been
// successfully processed (see SeenDelivery).
func (s *Store) HasDelivery(id string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM deliveries WHERE id = ?`, id).Scan(&n); err != nil {
		return false, fmt.Errorf("has delivery: %w", err)
	}
	return n > 0, nil
}

// SeenDelivery records a GitHub delivery id (the post-ingest "processed" mark)
// and reports whether the row was newly inserted. Idempotent via INSERT OR IGNORE.
func (s *Store) SeenDelivery(id, event string) (bool, error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO deliveries (id, event, received_at) VALUES (?, ?, ?)`,
		id, event, time.Now().Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("record delivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delivery rows: %w", err)
	}
	return n == 1, nil
}

// RecordRound upserts the latest-seen timestamp for a PR head SHA (a Round).
func (s *Store) RecordRound(owner, repo string, number int, sha string) error {
	_, err := s.db.Exec(
		`INSERT INTO rounds (owner, repo, number, sha, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(owner, repo, number, sha) DO UPDATE SET updated_at = excluded.updated_at`,
		owner, repo, number, sha, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record round: %w", err)
	}
	return nil
}

// RoundExists reports whether a Round row exists for the given key.
func (s *Store) RoundExists(owner, repo string, number int, sha string) (bool, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM rounds WHERE owner = ? AND repo = ? AND number = ? AND sha = ?`,
		owner, repo, number, sha,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("round exists: %w", err)
	}
	return n > 0, nil
}

// PendingItem is a stored latest-state summary for one PR signal-type (ADR-0006).
type PendingItem struct {
	Owner      string
	Repo       string
	Number     int
	SignalType string
	SHA        string
	PRState    string
	Summary    string
	UpdatedAt  int64
}

// UpsertPending stores p as the latest pending item for its
// (owner, repo, number, signal_type); a newer write replaces the prior one
// (ADR-0006). The stored timestamp is set to now.
func (s *Store) UpsertPending(p PendingItem) error {
	ctx, span := otel.Tracer("caw-hub/store").Start(context.Background(), "store.upsert_pending")
	defer span.End()
	span.SetAttributes(
		attribute.String("github.owner", p.Owner),
		attribute.String("github.repo", p.Repo),
		attribute.Int("github.pr_number", p.Number),
		attribute.String("store.signal_type", p.SignalType),
	)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending (owner, repo, number, signal_type, sha, pr_state, summary, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(owner, repo, number, signal_type) DO UPDATE SET
		     sha = excluded.sha, pr_state = excluded.pr_state,
		     summary = excluded.summary, updated_at = excluded.updated_at`,
		p.Owner, p.Repo, p.Number, p.SignalType, p.SHA, p.PRState, p.Summary, time.Now().Unix(),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "upsert pending")
		return fmt.Errorf("upsert pending: %w", err)
	}
	return nil
}

// ListPending returns all stored pending items in a stable order.
func (s *Store) ListPending() ([]PendingItem, error) {
	rows, err := s.db.Query(
		`SELECT owner, repo, number, signal_type, sha, pr_state, summary, updated_at
		 FROM pending ORDER BY owner, repo, number, signal_type`,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []PendingItem
	for rows.Next() {
		var p PendingItem
		if err := rows.Scan(
			&p.Owner, &p.Repo, &p.Number, &p.SignalType,
			&p.SHA, &p.PRState, &p.Summary, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

// ListPendingForInstallation returns pending items whose repo belongs to the
// given installation. It performs an inner JOIN against installation_repos so
// that only items for repos associated with installationID are returned.
func (s *Store) ListPendingForInstallation(installationID string) ([]PendingItem, error) {
	rows, err := s.db.Query(
		`SELECT p.owner, p.repo, p.number, p.signal_type, p.sha, p.pr_state, p.summary, p.updated_at
		 FROM pending p
		 JOIN installation_repos ir
		   ON ir.full_name = p.owner || '/' || p.repo
		 WHERE ir.installation_id = ?
		 ORDER BY p.owner, p.repo, p.number, p.signal_type`,
		installationID,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending for installation: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var items []PendingItem
	for rows.Next() {
		var p PendingItem
		if err := rows.Scan(
			&p.Owner, &p.Repo, &p.Number, &p.SignalType,
			&p.SHA, &p.PRState, &p.Summary, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending for installation: %w", err)
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

// Signal is one observed feedback item for a Round, awaiting compilation.
type Signal struct {
	Owner      string
	Repo       string
	Number     int
	SHA        string
	SignalType string
	Source     string
	ExternalID string
	Severity   string
	Body       string
	UpdatedAt  int64
}

// AddSignal records a signal for a Round, replacing any prior signal with the
// same natural key (owner/repo/number/sha + signal_type + source + external_id).
func (s *Store) AddSignal(sig Signal) error {
	_, err := s.db.Exec(
		`INSERT INTO signals
		     (owner, repo, number, sha, signal_type, source, external_id, severity, body, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(owner, repo, number, sha, signal_type, source, external_id) DO UPDATE SET
		     severity = excluded.severity, body = excluded.body, updated_at = excluded.updated_at`,
		sig.Owner, sig.Repo, sig.Number, sig.SHA, sig.SignalType,
		sig.Source, sig.ExternalID, sig.Severity, sig.Body, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("add signal: %w", err)
	}
	return nil
}

// SignalsForRound returns every signal stored for a PR head SHA, in a stable order.
func (s *Store) SignalsForRound(owner, repo string, number int, sha string) ([]Signal, error) {
	rows, err := s.db.Query(
		`SELECT owner, repo, number, sha, signal_type, source, external_id, severity, body, updated_at
		 FROM signals WHERE owner = ? AND repo = ? AND number = ? AND sha = ?
		 ORDER BY signal_type, source, external_id`,
		owner, repo, number, sha,
	)
	if err != nil {
		return nil, fmt.Errorf("signals for round: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Signal
	for rows.Next() {
		var sig Signal
		if err := rows.Scan(
			&sig.Owner, &sig.Repo, &sig.Number, &sig.SHA, &sig.SignalType,
			&sig.Source, &sig.ExternalID, &sig.Severity, &sig.Body, &sig.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan signal: %w", err)
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// LatestRoundSHA returns the most-recently-seen head SHA for a PR, used to
// attach signals (e.g. issue comments) that don't carry a SHA themselves.
func (s *Store) LatestRoundSHA(owner, repo string, number int) (sha string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT sha FROM rounds WHERE owner = ? AND repo = ? AND number = ?
		 ORDER BY updated_at DESC, rowid DESC LIMIT 1`,
		owner, repo, number,
	).Scan(&sha)
	switch {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("latest round sha: %w", err)
	default:
		return sha, true, nil
	}
}

// Token is one row of the tokens table (ADR-0003 + Auth v2 user-binding fields).
//
// Phase 1 widens the row but does not yet enforce the user check. Legacy rows
// minted before Auth v2 (and rows still minted by the install-callback and
// installation.created webhook paths) carry GitHubUserID == nil and
// DeviceLabel == "legacy" / "installation-auto"; Phase 2's RequireRepoAccess
// skips them. Phase 3 mints user-bound rows where GitHubUserID is set.
type Token struct {
	ID              string // 26-char ULID; legacy rows are backfilled to "legacy-<rowid>" on first VerifyToken read
	Hash            string // hex sha256 of the raw token
	InstallationID  string
	Org             string
	GitHubUserID    *int64 // nil = legacy / not yet user-bound
	GitHubUserLogin string
	DeviceLabel     string // "legacy" | "installation-auto" | "manifest-setup" | user-supplied label
	CreatedAt       int64  // Unix epoch seconds
	ExpiresAt       *int64 // nil = no expiry (legacy)
	LastUsedAt      *int64
	RevokedAt       *int64
}

// InsertTokenRow writes a Token row in its full Auth v2 shape. Callers MUST
// supply Hash, InstallationID and DeviceLabel; ID is auto-generated only when
// empty; CreatedAt is set to now when zero. ON CONFLICT(token_hash) updates
// every mutable column so re-minting the same hash overwrites stale metadata.
func (s *Store) InsertTokenRow(t Token) error {
	if t.Hash == "" {
		return fmt.Errorf("insert token row: Hash is required")
	}
	if t.InstallationID == "" {
		return fmt.Errorf("insert token row: InstallationID is required")
	}
	if t.DeviceLabel == "" {
		t.DeviceLabel = "legacy"
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(
		`INSERT INTO tokens (token_hash, installation_id, org, created_at,
		                     id, github_user_id, github_user_login, device_label,
		                     expires_at, last_used_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET
		     installation_id   = excluded.installation_id,
		     org               = excluded.org,
		     id                = excluded.id,
		     github_user_id    = excluded.github_user_id,
		     github_user_login = excluded.github_user_login,
		     device_label      = excluded.device_label,
		     expires_at        = excluded.expires_at,
		     last_used_at      = excluded.last_used_at,
		     revoked_at        = excluded.revoked_at`,
		t.Hash, t.InstallationID, t.Org, t.CreatedAt,
		t.ID, nullableInt(t.GitHubUserID), nullableString(t.GitHubUserLogin), t.DeviceLabel,
		nullableInt(t.ExpiresAt), nullableInt(t.LastUsedAt), nullableInt(t.RevokedAt),
	)
	if err != nil {
		return fmt.Errorf("insert token row: %w", err)
	}
	return nil
}

// InsertToken stores a hashed installation token (ADR-0003) in legacy shape.
//
// Deprecated: use InsertTokenRow with a fully populated Token. This wrapper is
// kept for one release so the v0.1 surface stays usable while the Auth v2 code
// paths land phase-by-phase; Phase 5 removes it.
func (s *Store) InsertToken(tokenHash, installationID, org string) error {
	return s.InsertTokenRow(Token{
		Hash:           tokenHash,
		InstallationID: installationID,
		Org:            org,
		DeviceLabel:    "legacy",
	})
}

// VerifyToken resolves a token hash to the full Token row. ok is false when the
// token is unknown, has been revoked, or is past its expiry. It satisfies the
// auth.Verifier interface.
//
// Filtering on revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now)
// is intentional: revocation and expiry MUST be checked on the hot path; the
// new token rows (Phase 3+) set them, legacy rows leave both NULL and remain
// usable.
//
// Legacy backfill: rows that pre-date the Auth v2 `id` column carry id = ”.
// First read populates id with "legacy-<rowid>" so downstream code (e.g.
// RevokeToken, ListTokensForUser) always has a stable identifier. Idempotent:
// the UPDATE is guarded on id IS NULL OR id = ” so re-reads are no-ops.
func (s *Store) VerifyToken(tokenHash string) (Token, bool, error) {
	var t Token
	var rowid int64
	var id, gul, dl sql.NullString
	var guid, exp, lu, rev sql.NullInt64
	now := time.Now().Unix()
	err := s.db.QueryRow(
		`SELECT rowid, token_hash, installation_id, org, created_at,
		        id, github_user_id, github_user_login, device_label,
		        expires_at, last_used_at, revoked_at
		 FROM tokens
		 WHERE token_hash = ?
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > ?)`,
		tokenHash, now,
	).Scan(
		&rowid, &t.Hash, &t.InstallationID, &t.Org, &t.CreatedAt,
		&id, &guid, &gul, &dl, &exp, &lu, &rev,
	)
	switch {
	case err == sql.ErrNoRows:
		return Token{}, false, nil
	case err != nil:
		return Token{}, false, fmt.Errorf("verify token: %w", err)
	}
	t.ID = id.String
	t.DeviceLabel = dl.String
	if t.DeviceLabel == "" {
		t.DeviceLabel = "legacy"
	}
	if guid.Valid {
		v := guid.Int64
		t.GitHubUserID = &v
	}
	t.GitHubUserLogin = gul.String
	if exp.Valid {
		v := exp.Int64
		t.ExpiresAt = &v
	}
	if lu.Valid {
		v := lu.Int64
		t.LastUsedAt = &v
	}
	if rev.Valid {
		v := rev.Int64
		t.RevokedAt = &v
	}
	if t.ID == "" {
		t.ID = fmt.Sprintf("legacy-%d", rowid)
		// Backfill — guarded so concurrent VerifyToken calls converge on the
		// same value (rowid is stable per row). Errors are not fatal: the
		// auth check has already succeeded, the legacy row is usable, and
		// the next read will retry the backfill.
		_, _ = s.db.Exec(
			`UPDATE tokens SET id = ? WHERE token_hash = ? AND (id IS NULL OR id = '')`,
			t.ID, tokenHash,
		)
	}
	return t, true, nil
}

// TouchTokenLastUsed sets the token's last_used_at to now. Best-effort: a zero
// rows-affected (row revoked / expired between auth and writeback) is not an
// error. Callers should debounce — once per 60s per token id — to avoid
// hot-row contention on busy tokens.
func (s *Store) TouchTokenLastUsed(tokenID string, now int64) error {
	if tokenID == "" {
		return nil
	}
	if _, err := s.db.Exec(
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`, now, tokenID,
	); err != nil {
		return fmt.Errorf("touch token: %w", err)
	}
	return nil
}

// RevokeToken sets revoked_at = now for the token with the given id. The row
// stays in the table for audit; VerifyToken's WHERE clause filters it out on
// the next presentation. Returns no error if the id matches no row — the
// caller has already authenticated and revoke is naturally idempotent.
func (s *Store) RevokeToken(tokenID string, now int64) error {
	if tokenID == "" {
		return fmt.Errorf("revoke token: id is required")
	}
	if _, err := s.db.Exec(
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now, tokenID,
	); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// ListTokensForUser returns every token row bound to the given github_user_id.
// Used by Phase 4's `/me/tokens` page and Phase 3.5's webhook fan-out. Revoked
// rows are included so the management UI can show them; callers filter as
// needed. Returns an empty slice when the user has no tokens (legacy callers
// with userID == 0 get zero rows, since no legacy row carries a user id).
func (s *Store) ListTokensForUser(userID int64) ([]Token, error) {
	rows, err := s.db.Query(
		`SELECT rowid, token_hash, installation_id, org, created_at,
		        id, github_user_id, github_user_login, device_label,
		        expires_at, last_used_at, revoked_at
		 FROM tokens
		 WHERE github_user_id = ?
		 ORDER BY created_at DESC, rowid DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tokens for user: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Token
	for rows.Next() {
		var t Token
		var rowid int64
		var id, gul, dl sql.NullString
		var guid, exp, lu, rev sql.NullInt64
		if err := rows.Scan(
			&rowid, &t.Hash, &t.InstallationID, &t.Org, &t.CreatedAt,
			&id, &guid, &gul, &dl, &exp, &lu, &rev,
		); err != nil {
			return nil, fmt.Errorf("list tokens scan: %w", err)
		}
		t.ID = id.String
		if t.ID == "" {
			t.ID = fmt.Sprintf("legacy-%d", rowid)
		}
		t.DeviceLabel = dl.String
		if guid.Valid {
			v := guid.Int64
			t.GitHubUserID = &v
		}
		t.GitHubUserLogin = gul.String
		if exp.Valid {
			v := exp.Int64
			t.ExpiresAt = &v
		}
		if lu.Valid {
			v := lu.Int64
			t.LastUsedAt = &v
		}
		if rev.Valid {
			v := rev.Int64
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens iterate: %w", err)
	}
	return out, nil
}

// GetTokenByID returns the token row keyed on id. ok is false when no row
// matches. Unlike VerifyToken this does NOT filter on revoked_at / expires_at
// — the Phase 4 management surface needs to surface revoked rows in /me/tokens
// and answer DELETE idempotently (re-DELETE of an already-revoked token reads
// the row, sees the caller still owns it, and returns 204).
func (s *Store) GetTokenByID(tokenID string) (Token, bool, error) {
	if tokenID == "" {
		return Token{}, false, nil
	}
	var t Token
	var rowid int64
	var id, gul, dl sql.NullString
	var guid, exp, lu, rev sql.NullInt64
	err := s.db.QueryRow(
		`SELECT rowid, token_hash, installation_id, org, created_at,
		        id, github_user_id, github_user_login, device_label,
		        expires_at, last_used_at, revoked_at
		 FROM tokens
		 WHERE id = ?`,
		tokenID,
	).Scan(
		&rowid, &t.Hash, &t.InstallationID, &t.Org, &t.CreatedAt,
		&id, &guid, &gul, &dl, &exp, &lu, &rev,
	)
	switch {
	case err == sql.ErrNoRows:
		return Token{}, false, nil
	case err != nil:
		return Token{}, false, fmt.Errorf("get token by id: %w", err)
	}
	t.ID = id.String
	if t.ID == "" {
		t.ID = fmt.Sprintf("legacy-%d", rowid)
	}
	t.DeviceLabel = dl.String
	if guid.Valid {
		v := guid.Int64
		t.GitHubUserID = &v
	}
	t.GitHubUserLogin = gul.String
	if exp.Valid {
		v := exp.Int64
		t.ExpiresAt = &v
	}
	if lu.Valid {
		v := lu.Int64
		t.LastUsedAt = &v
	}
	if rev.Valid {
		v := rev.Int64
		t.RevokedAt = &v
	}
	return t, true, nil
}

// RevokeAllTokensForUser sets revoked_at = now on every still-active token
// bound to userID. Idempotent: rows already revoked are not touched (their
// original revoked_at stays the audit-of-record timestamp). Returns the
// number of rows newly revoked by this call.
//
// Phase 4's POST /me/recover panic button: one transaction, every token
// belonging to the user dies at the same wall-clock second so a leaked token
// cannot keep working at the millisecond a sibling token revoked.
func (s *Store) RevokeAllTokensForUser(userID int64, now int64) (int, error) {
	if userID == 0 {
		// Defensive: userID 0 is the legacy-token sentinel. Revoking "every
		// legacy token in the table" is never what /me/recover wants.
		return 0, nil
	}
	res, err := s.db.Exec(
		`UPDATE tokens SET revoked_at = ?
		 WHERE github_user_id = ? AND revoked_at IS NULL`,
		now, userID,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke all tokens for user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke all tokens for user rows: %w", err)
	}
	return int(n), nil
}

// RevokeTokensForInstallation sets revoked_at = now on every still-active
// token bound to installationID. Called from the installation.deleted
// webhook handler: when GitHub tells us the App was uninstalled, the cached
// repo-access decisions are flushed (Phase 2) AND the tokens themselves are
// revoked at the persistence layer (Phase 4) — defense in depth.
//
// Returns the number of rows newly revoked. installationID == "" is a no-op
// (never matches a real row).
func (s *Store) RevokeTokensForInstallation(installationID string, now int64) (int, error) {
	if installationID == "" {
		return 0, nil
	}
	res, err := s.db.Exec(
		`UPDATE tokens SET revoked_at = ?
		 WHERE installation_id = ? AND revoked_at IS NULL`,
		now, installationID,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke tokens for installation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke tokens for installation rows: %w", err)
	}
	return int(n), nil
}

// nullableInt converts a *int64 to a database value: nil pointer → SQL NULL,
// non-nil → its dereferenced value.
func nullableInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullableString returns SQL NULL for an empty string. The Token type uses
// "" rather than *string for GitHubUserLogin to keep the API ergonomic.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UpsertInstallation inserts or replaces a GitHub App installation record.
func (s *Store) UpsertInstallation(installationID, accountLogin string) error {
	_, err := s.db.Exec(
		`INSERT INTO installations (installation_id, account_login, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(installation_id) DO UPDATE SET account_login = excluded.account_login`,
		installationID, accountLogin, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert installation: %w", err)
	}
	return nil
}

// UpdateInstallationAppSlug records the GitHub App's URL slug for an
// installation. Called from the webhook ingest of installation.created and
// from the manifest-flow callback so the Auth v2 redirect-to-install path
// (Phase 3) can build https://github.com/apps/<slug>/installations/new for an
// unauthenticated user mid-OAuth. A blank slug is allowed (no-op write).
func (s *Store) UpdateInstallationAppSlug(installationID, slug string) error {
	if installationID == "" {
		return fmt.Errorf("update installation app_slug: installation_id is required")
	}
	if _, err := s.db.Exec(
		`UPDATE installations SET app_slug = ? WHERE installation_id = ?`,
		slug, installationID,
	); err != nil {
		return fmt.Errorf("update installation app_slug: %w", err)
	}
	return nil
}

// AnyAppSlug returns a non-empty app_slug from any installations row, or "" if
// none has one yet. All installations of the same App share the same slug, so
// the first non-empty wins. Used by Auth v2's /auth/cb/github handler when it
// needs to redirect a user with zero installations to the App install URL —
// the env CAW_APP_SLUG override always wins over this fallback.
func (s *Store) AnyAppSlug() (string, error) {
	var slug string
	err := s.db.QueryRow(
		`SELECT app_slug FROM installations WHERE app_slug != '' ORDER BY rowid LIMIT 1`,
	).Scan(&slug)
	switch {
	case err == sql.ErrNoRows:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("any app slug: %w", err)
	}
	return slug, nil
}

// DeleteInstallation removes an installation and all its repo associations.
func (s *Store) DeleteInstallation(installationID string) error {
	if _, err := s.db.Exec(
		`DELETE FROM installation_repos WHERE installation_id = ?`, installationID,
	); err != nil {
		return fmt.Errorf("delete installation repos: %w", err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM installations WHERE installation_id = ?`, installationID,
	); err != nil {
		return fmt.Errorf("delete installation: %w", err)
	}
	return nil
}

// AddInstallationRepo associates a repository with an installation.
func (s *Store) AddInstallationRepo(installationID, fullName string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO installation_repos (installation_id, full_name) VALUES (?, ?)`,
		installationID, fullName,
	)
	if err != nil {
		return fmt.Errorf("add installation repo: %w", err)
	}
	return nil
}

// RemoveInstallationRepo disassociates a repository from an installation.
func (s *Store) RemoveInstallationRepo(installationID, fullName string) error {
	_, err := s.db.Exec(
		`DELETE FROM installation_repos WHERE installation_id = ? AND full_name = ?`,
		installationID, fullName,
	)
	if err != nil {
		return fmt.Errorf("remove installation repo: %w", err)
	}
	return nil
}

// RepoInInstallation reports whether a repo is associated with an installation.
func (s *Store) RepoInInstallation(installationID, fullName string) (bool, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM installation_repos WHERE installation_id = ? AND full_name = ?`,
		installationID, fullName,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("repo in installation: %w", err)
	}
	return n > 0, nil
}

// InstallationForRepo returns the installation ID that owns a repo (full name
// "owner/repo"). ok is false when no installation is associated with the repo.
func (s *Store) InstallationForRepo(fullName string) (string, bool, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT installation_id FROM installation_repos WHERE full_name = ? LIMIT 1`,
		fullName,
	).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("installation for repo: %w", err)
	}
	return id, true, nil
}

// AppCredentials holds the persisted GitHub App credentials (manifest flow).
type AppCredentials struct {
	AppID         string
	ClientID      string
	ClientSecret  string
	WebhookSecret string
	PEM           string
}

// SaveAppCredentials persists (or replaces) the single set of App credentials.
func (s *Store) SaveAppCredentials(c AppCredentials) error {
	_, err := s.db.Exec(
		`INSERT INTO app_credentials (id, app_id, client_id, client_secret, webhook_secret, pem, created_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     app_id = excluded.app_id, client_id = excluded.client_id,
		     client_secret = excluded.client_secret, webhook_secret = excluded.webhook_secret,
		     pem = excluded.pem, created_at = excluded.created_at`,
		c.AppID, c.ClientID, c.ClientSecret, c.WebhookSecret, c.PEM, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("save app credentials: %w", err)
	}
	return nil
}

// LoadAppCredentials retrieves the persisted App credentials.
// ok is false when none have been saved yet.
func (s *Store) LoadAppCredentials() (AppCredentials, bool, error) {
	var c AppCredentials
	err := s.db.QueryRow(
		`SELECT app_id, client_id, client_secret, webhook_secret, pem FROM app_credentials WHERE id = 1`,
	).Scan(&c.AppID, &c.ClientID, &c.ClientSecret, &c.WebhookSecret, &c.PEM)
	switch {
	case err == sql.ErrNoRows:
		return AppCredentials{}, false, nil
	case err != nil:
		return AppCredentials{}, false, fmt.Errorf("load app credentials: %w", err)
	default:
		return c, true, nil
	}
}

// Lease represents the single-owner rebase-lease for a PR (ADR-0005).
// Only one holder may hold a lease for a given (Org, Repo, PRNumber) at any time.
type Lease struct {
	Org             string
	Repo            string
	PRNumber        int
	Holder          string // installation_id of current holder
	ExpiresAt       int64  // Unix epoch seconds
	LastHeartbeatAt int64  // Unix epoch seconds
	AcquiredAt      int64  // Unix epoch seconds
}

// AcquireLeaseResult reports the outcome of an AcquireLease call.
type AcquireLeaseResult struct {
	Granted bool
	Lease   Lease
}

// AcquireLease attempts to grant holder a lease for the given PR.
// If the PR already has a non-expired lease held by a different holder the
// grant is denied and the current holder's lease is returned with Granted=false.
// If the PR has no lease, or the existing lease is expired, a new lease is
// written (replacing any stale row) and Granted=true is returned.
// leaseDuration is added to now to compute ExpiresAt.
func (s *Store) AcquireLease(org, repo string, prNumber int, holder string, leaseDuration int64) (AcquireLeaseResult, error) {
	now := time.Now().Unix()
	expires := now + leaseDuration

	// Try an insert first (happy path: no current lease).
	_, err := s.db.Exec(
		`INSERT INTO leases (org, repo, pr_number, holder, expires_at, last_heartbeat_at, acquired_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org, repo, pr_number) DO UPDATE SET
		     holder            = excluded.holder,
		     expires_at        = excluded.expires_at,
		     last_heartbeat_at = excluded.last_heartbeat_at,
		     acquired_at       = excluded.acquired_at
		 WHERE leases.expires_at < ?`,
		org, repo, prNumber, holder, expires, now, now,
		now, // WHERE clause: only replace if expired
	)
	if err != nil {
		return AcquireLeaseResult{}, fmt.Errorf("acquire lease: %w", err)
	}

	// Read back the current row to determine who actually holds it.
	l, ok, err := s.GetLease(org, repo, prNumber)
	if err != nil {
		return AcquireLeaseResult{}, err
	}
	if !ok {
		// Shouldn't happen — we just wrote a row.
		return AcquireLeaseResult{}, fmt.Errorf("acquire lease: row disappeared after write")
	}

	return AcquireLeaseResult{
		Granted: l.Holder == holder && l.ExpiresAt == expires,
		Lease:   l,
	}, nil
}

// GetLease retrieves the current lease for the given PR.
// ok is false when no lease row exists.
func (s *Store) GetLease(org, repo string, prNumber int) (Lease, bool, error) {
	var l Lease
	err := s.db.QueryRow(
		`SELECT org, repo, pr_number, holder, expires_at, last_heartbeat_at, acquired_at
		 FROM leases WHERE org = ? AND repo = ? AND pr_number = ?`,
		org, repo, prNumber,
	).Scan(&l.Org, &l.Repo, &l.PRNumber, &l.Holder, &l.ExpiresAt, &l.LastHeartbeatAt, &l.AcquiredAt)
	switch {
	case err == sql.ErrNoRows:
		return Lease{}, false, nil
	case err != nil:
		return Lease{}, false, fmt.Errorf("get lease: %w", err)
	default:
		return l, true, nil
	}
}

// RenewLease extends expires_at and updates last_heartbeat_at for an active lease.
// Only the current holder may renew; the lease must not be expired.
// extendBy is added to now to compute the new expires_at (≥ 1 is required).
// Returns the updated lease on success. Returns an error if the lease does not
// exist, is expired, or holder does not match.
func (s *Store) RenewLease(org, repo string, prNumber int, holder string, extendBy int64) (Lease, error) {
	now := time.Now().Unix()
	newExpires := now + extendBy
	res, err := s.db.Exec(
		`UPDATE leases
		 SET expires_at = ?, last_heartbeat_at = ?
		 WHERE org = ? AND repo = ? AND pr_number = ? AND holder = ? AND expires_at >= ?`,
		newExpires, now,
		org, repo, prNumber, holder, now,
	)
	if err != nil {
		return Lease{}, fmt.Errorf("renew lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Lease{}, fmt.Errorf("renew lease: rows affected: %w", err)
	}
	if n == 0 {
		return Lease{}, fmt.Errorf("renew lease: no matching active lease for holder %q", holder)
	}
	l, ok, err := s.GetLease(org, repo, prNumber)
	if err != nil {
		return Lease{}, fmt.Errorf("renew lease: read back: %w", err)
	}
	if !ok {
		return Lease{}, fmt.Errorf("renew lease: lease disappeared after update")
	}
	return l, nil
}

// ReleaseLease deletes the lease for the given PR.
// Only the current holder may release their own lease.
// Returns an error if no matching lease exists (not held by holder, already
// released, or never acquired).
func (s *Store) ReleaseLease(org, repo string, prNumber int, holder string) error {
	res, err := s.db.Exec(
		`DELETE FROM leases WHERE org = ? AND repo = ? AND pr_number = ? AND holder = ?`,
		org, repo, prNumber, holder,
	)
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release lease: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("release lease: no lease held by %q for %s/%s#%d", holder, org, repo, prNumber)
	}
	return nil
}
