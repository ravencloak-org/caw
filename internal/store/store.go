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
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
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

// InsertToken stores a hashed installation token (ADR-0003).
func (s *Store) InsertToken(tokenHash, installationID, org string) error {
	_, err := s.db.Exec(
		`INSERT INTO tokens (token_hash, installation_id, org, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET installation_id = excluded.installation_id, org = excluded.org`,
		tokenHash, installationID, org, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}
	return nil
}

// VerifyToken resolves a token hash to its installation id. ok is false when the
// token is unknown. It satisfies the auth.Verifier interface.
func (s *Store) VerifyToken(tokenHash string) (installationID string, ok bool, err error) {
	err = s.db.QueryRow(
		`SELECT installation_id FROM tokens WHERE token_hash = ?`, tokenHash,
	).Scan(&installationID)
	switch {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("verify token: %w", err)
	default:
		return installationID, true, nil
	}
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
