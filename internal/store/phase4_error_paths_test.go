package store

import (
	"strings"
	"testing"
)

// Phase 4 added GetTokenByID / RevokeAllTokensForUser /
// RevokeTokensForInstallation. Each has an `err != nil` branch on its DB
// call that the happy-path tests can't reach. Drive those branches by
// closing the store mid-flight — every query then fails with sql.ErrConnDone
// (or equivalent), letting the error wrapper fire.

func closedStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return s
}

func TestGetTokenByID_DBClosedReturnsError(t *testing.T) {
	s := closedStore(t)
	_, _, err := s.GetTokenByID("some-id")
	if err == nil {
		t.Fatal("want error on closed DB, got nil")
	}
	if !strings.Contains(err.Error(), "get token by id") {
		t.Errorf("want wrapped 'get token by id' error, got %v", err)
	}
}

func TestGetTokenByID_EmptyIDIsNotFound(t *testing.T) {
	// Empty id MUST short-circuit to (Token{}, false, nil) WITHOUT touching
	// the DB — otherwise a stray empty id could hit a prepared-statement
	// pool exhaustion before the query even runs. Covers the early-return
	// branch on a live store.
	s := newTestStore(t)
	tok, ok, err := s.GetTokenByID("")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
	if tok.ID != "" {
		t.Errorf("got non-zero token: %+v", tok)
	}
}

func TestRevokeAllTokensForUser_DBClosedReturnsError(t *testing.T) {
	s := closedStore(t)
	_, err := s.RevokeAllTokensForUser(7777, 1_700_000_000)
	if err == nil {
		t.Fatal("want error on closed DB, got nil")
	}
	if !strings.Contains(err.Error(), "revoke all tokens for user") {
		t.Errorf("want wrapped 'revoke all tokens for user' error, got %v", err)
	}
}

func TestRevokeAllTokensForUser_UserID0IsNoop(t *testing.T) {
	// userID 0 MUST short-circuit to (0, nil) — the legacy-token sentinel
	// must never accidentally match every legacy row. Covers the
	// defensive early-return branch.
	s := newTestStore(t)
	n, err := s.RevokeAllTokensForUser(0, 1_700_000_000)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestRevokeTokensForInstallation_DBClosedReturnsError(t *testing.T) {
	s := closedStore(t)
	_, err := s.RevokeTokensForInstallation("inst-1", 1_700_000_000)
	if err == nil {
		t.Fatal("want error on closed DB, got nil")
	}
	if !strings.Contains(err.Error(), "revoke tokens for installation") {
		t.Errorf("want wrapped 'revoke tokens for installation' error, got %v", err)
	}
}

func TestRevokeTokensForInstallation_EmptyInstallIDIsNoop(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RevokeTokensForInstallation("", 1_700_000_000)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestRevokeTokensForInstallation_RevokesActiveLeavesAlreadyRevoked(t *testing.T) {
	// Exercises the happy + idempotent-skip path together: insert two
	// tokens for one installation, revoke the first manually, then call
	// RevokeTokensForInstallation. Only the still-active row should
	// receive a new revoked_at — the already-revoked row keeps its
	// original timestamp (revoked_at IS NULL guard in the WHERE clause).
	s := newTestStore(t)
	uid := int64(42)
	if err := s.InsertTokenRow(Token{
		ID: "tok-active", Hash: "h-active",
		InstallationID: "inst-x", Org: "acme",
		DeviceLabel: "device-a", GitHubUserID: &uid, GitHubUserLogin: "alice",
	}); err != nil {
		t.Fatalf("insert active: %v", err)
	}
	if err := s.InsertTokenRow(Token{
		ID: "tok-old-rev", Hash: "h-old",
		InstallationID: "inst-x", Org: "acme",
		DeviceLabel: "device-b", GitHubUserID: &uid, GitHubUserLogin: "alice",
	}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	// Pre-revoke tok-old-rev so RevokeTokensForInstallation's WHERE clause
	// excludes it.
	oldRevAt := int64(1_700_000_000)
	if err := s.RevokeToken("tok-old-rev", oldRevAt); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	now := int64(1_700_001_000)
	n, err := s.RevokeTokensForInstallation("inst-x", now)
	if err != nil {
		t.Fatalf("RevokeTokensForInstallation: %v", err)
	}
	if n != 1 {
		t.Errorf("rows revoked = %d, want 1 (only tok-active)", n)
	}
	// Verify tok-active now revoked, tok-old-rev unchanged.
	if got, _, _ := s.GetTokenByID("tok-active"); got.RevokedAt == nil || *got.RevokedAt != now {
		t.Errorf("tok-active.RevokedAt = %v, want %d", got.RevokedAt, now)
	}
	if got, _, _ := s.GetTokenByID("tok-old-rev"); got.RevokedAt == nil || *got.RevokedAt != oldRevAt {
		t.Errorf("tok-old-rev.RevokedAt = %v, want %d (unchanged)", got.RevokedAt, oldRevAt)
	}
}

func TestRevokeAllTokensForUser_RevokesActiveOnly(t *testing.T) {
	// Same pattern: revoke-all must skip already-revoked rows.
	s := newTestStore(t)
	uid := int64(99)
	for _, tc := range []struct {
		id          string
		preRevoke   bool
		preRevokeAt int64
	}{
		{"tok-A", false, 0},
		{"tok-B", true, 1_700_000_000},
		{"tok-C", false, 0},
	} {
		if err := s.InsertTokenRow(Token{
			ID: tc.id, Hash: "h-" + tc.id,
			InstallationID: "inst-y", Org: "beta",
			DeviceLabel: tc.id, GitHubUserID: &uid, GitHubUserLogin: "bob",
		}); err != nil {
			t.Fatalf("insert %s: %v", tc.id, err)
		}
		if tc.preRevoke {
			if err := s.RevokeToken(tc.id, tc.preRevokeAt); err != nil {
				t.Fatalf("pre-revoke %s: %v", tc.id, err)
			}
		}
	}
	now := int64(1_700_005_000)
	n, err := s.RevokeAllTokensForUser(uid, now)
	if err != nil {
		t.Fatalf("RevokeAllTokensForUser: %v", err)
	}
	if n != 2 {
		t.Errorf("rows revoked = %d, want 2 (A + C, not B)", n)
	}
	// B should still carry its original timestamp.
	if got, _, _ := s.GetTokenByID("tok-B"); got.RevokedAt == nil || *got.RevokedAt != 1_700_000_000 {
		t.Errorf("tok-B.RevokedAt = %v, want 1_700_000_000 (unchanged)", got.RevokedAt)
	}
}
