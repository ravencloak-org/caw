// Package store — auth_sessions table CRUD (Auth v2, Phases 3+).
//
// An AuthSession is a short-lived (≤10 min) row created by POST /auth/start
// that anchors an MCP-initiated `login` handoff: it stores the PKCE challenge,
// the chosen handoff mode (loopback or device), and the eventual TokenBundle
// the device-flow poller picks up. Phase 1 lands the schema + this CRUD
// surface only — no handler reads it yet. Phase 3 wires `/auth/start`,
// `/auth/u/:session_id`, `/auth/cb/github`, `/auth/picker/:session_id` and
// `/auth/poll` on top.
package store

import (
	"database/sql"
	"fmt"
)

// AuthSession is one row of the auth_sessions table.
//
// Pointer fields signal SQL NULL (GitHubUserID before OAuth completes).
// String fields use "" to signal NULL for ergonomic Go-side handling; the
// store layer converts to/from SQL NULL on the wire.
type AuthSession struct {
	ID                  string // 26-char ULID
	HandshakeMode       string // "loopback" | "device"
	CodeChallenge       string // S256 hash, base64url
	CodeChallengeMethod string // "S256"
	LoopbackRedirect    string // empty for device mode
	DeviceCode          string // empty for loopback mode
	UserCode            string // empty for loopback mode; user-typed for device
	ClientLabel         string // human-readable device label (≤64 chars)
	GitHubUserID        *int64 // populated after OAuth callback
	GitHubUserLogin     string
	PendingBundleJSON   string // TokenBundle awaiting pickup (device flow)
	State               string // pending|awaiting_install|awaiting_picker|delivered|canceled|expired
	CreatedAt           int64
	ExpiresAt           int64
}

// InsertAuthSession writes a new session row. ID, HandshakeMode, CodeChallenge,
// CodeChallengeMethod, ClientLabel, CreatedAt and ExpiresAt are required.
// State defaults to "pending" when empty.
func (s *Store) InsertAuthSession(a AuthSession) error {
	if a.ID == "" {
		return fmt.Errorf("insert auth session: ID is required")
	}
	if a.HandshakeMode == "" {
		return fmt.Errorf("insert auth session: HandshakeMode is required")
	}
	if a.CodeChallenge == "" {
		return fmt.Errorf("insert auth session: CodeChallenge is required")
	}
	if a.CodeChallengeMethod == "" {
		return fmt.Errorf("insert auth session: CodeChallengeMethod is required")
	}
	if a.ClientLabel == "" {
		return fmt.Errorf("insert auth session: ClientLabel is required")
	}
	if a.CreatedAt == 0 {
		return fmt.Errorf("insert auth session: CreatedAt is required")
	}
	if a.ExpiresAt == 0 {
		return fmt.Errorf("insert auth session: ExpiresAt is required")
	}
	if a.State == "" {
		a.State = "pending"
	}
	_, err := s.db.Exec(
		`INSERT INTO auth_sessions (
		     id, handshake_mode, code_challenge, code_challenge_method,
		     loopback_redirect, device_code, user_code, client_label,
		     github_user_id, github_user_login, pending_bundle_json,
		     state, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.HandshakeMode, a.CodeChallenge, a.CodeChallengeMethod,
		nullableString(a.LoopbackRedirect), nullableString(a.DeviceCode),
		nullableString(a.UserCode), a.ClientLabel,
		nullableInt(a.GitHubUserID), nullableString(a.GitHubUserLogin),
		nullableString(a.PendingBundleJSON),
		a.State, a.CreatedAt, a.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("insert auth session: %w", err)
	}
	return nil
}

// GetAuthSession reads a session by id. ok is false when no row matches.
// A row past its expires_at is returned anyway; expiry is the caller's
// policy concern (the purger deletes them, the handler may also fail-fast).
func (s *Store) GetAuthSession(id string) (AuthSession, bool, error) {
	var a AuthSession
	var lr, dc, uc, gul, bundle sql.NullString
	var guid sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, handshake_mode, code_challenge, code_challenge_method,
		        loopback_redirect, device_code, user_code, client_label,
		        github_user_id, github_user_login, pending_bundle_json,
		        state, created_at, expires_at
		 FROM auth_sessions WHERE id = ?`, id,
	).Scan(
		&a.ID, &a.HandshakeMode, &a.CodeChallenge, &a.CodeChallengeMethod,
		&lr, &dc, &uc, &a.ClientLabel,
		&guid, &gul, &bundle,
		&a.State, &a.CreatedAt, &a.ExpiresAt,
	)
	switch {
	case err == sql.ErrNoRows:
		return AuthSession{}, false, nil
	case err != nil:
		return AuthSession{}, false, fmt.Errorf("get auth session: %w", err)
	}
	a.LoopbackRedirect = lr.String
	a.DeviceCode = dc.String
	a.UserCode = uc.String
	a.GitHubUserLogin = gul.String
	a.PendingBundleJSON = bundle.String
	if guid.Valid {
		v := guid.Int64
		a.GitHubUserID = &v
	}
	return a, true, nil
}

// UpdateAuthSessionState transitions a session to newState. It is the only
// post-insert mutator Phase 1 needs; Phases 3+ extend with state-shape-aware
// helpers (SetSessionUser, SetSessionPendingBundle, …) as the handlers land.
//
// No-op on a missing id — callers have already proven the session exists by a
// prior GetAuthSession.
func (s *Store) UpdateAuthSessionState(id, newState string) error {
	if id == "" {
		return fmt.Errorf("update auth session state: id is required")
	}
	if newState == "" {
		return fmt.Errorf("update auth session state: state is required")
	}
	if _, err := s.db.Exec(
		`UPDATE auth_sessions SET state = ? WHERE id = ?`, newState, id,
	); err != nil {
		return fmt.Errorf("update auth session state: %w", err)
	}
	return nil
}

// DeleteExpiredAuthSessions removes rows whose expires_at is at or before now.
// Returns the number of rows deleted. Driven by a 15-min purger in Phase 3+.
func (s *Store) DeleteExpiredAuthSessions(now int64) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM auth_sessions WHERE expires_at <= ?`, now,
	)
	if err != nil {
		return 0, fmt.Errorf("delete expired auth sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired auth sessions: rows affected: %w", err)
	}
	return n, nil
}
