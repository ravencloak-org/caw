// Package store is the Hub's SQLite-backed persistence layer.
//
// It uses modernc.org/sqlite — a pure-Go (cgo-free) driver — so the Hub
// compiles to a single static binary (ADR-0001).
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"time"

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
	_, err := s.db.Exec(
		`INSERT INTO pending (owner, repo, number, signal_type, sha, pr_state, summary, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(owner, repo, number, signal_type) DO UPDATE SET
		     sha = excluded.sha, pr_state = excluded.pr_state,
		     summary = excluded.summary, updated_at = excluded.updated_at`,
		p.Owner, p.Repo, p.Number, p.SignalType, p.SHA, p.PRState, p.Summary, time.Now().Unix(),
	)
	if err != nil {
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
