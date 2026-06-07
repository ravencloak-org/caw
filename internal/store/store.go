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

	_ "modernc.org/sqlite"
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

// SeenDelivery records a GitHub delivery id and reports whether it is new.
// It returns true the first time an id is seen, false for duplicates.
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
