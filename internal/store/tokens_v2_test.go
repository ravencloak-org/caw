package store

import (
	"strings"
	"testing"
	"time"
)

// Tests for the Phase 1 additions to store.go: TouchTokenLastUsed,
// RevokeToken, ListTokensForUser, UpdateInstallationAppSlug. Round-trip
// the new Token columns through InsertTokenRow + VerifyToken edge paths
// that store_test.go's happy-path doesn't cover.

func insertSampleToken(t *testing.T, s *Store, tok Token) Token {
	t.Helper()
	if tok.Hash == "" {
		tok.Hash = "hash-" + tok.InstallationID + "-" + tok.DeviceLabel
	}
	if tok.InstallationID == "" {
		tok.InstallationID = "inst1"
	}
	if tok.Org == "" {
		tok.Org = "org1"
	}
	if err := s.InsertTokenRow(tok); err != nil {
		t.Fatalf("InsertTokenRow: %v", err)
	}
	return tok
}

func TestTouchTokenLastUsed(t *testing.T) {
	s := newTestStore(t)
	uid := int64(7)
	insertSampleToken(t, s, Token{
		ID: "tok-touch-1", DeviceLabel: "device-a",
		GitHubUserID: &uid, GitHubUserLogin: "alice",
	})

	now := time.Now().Unix()
	if err := s.TouchTokenLastUsed("tok-touch-1", now); err != nil {
		t.Fatalf("TouchTokenLastUsed: %v", err)
	}

	rows, err := s.ListTokensForUser(uid)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListTokensForUser: rows=%d err=%v", len(rows), err)
	}
	if rows[0].LastUsedAt == nil || *rows[0].LastUsedAt != now {
		t.Errorf("LastUsedAt = %v, want %d", rows[0].LastUsedAt, now)
	}
}

func TestTouchTokenLastUsed_EmptyIDIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.TouchTokenLastUsed("", time.Now().Unix()); err != nil {
		t.Errorf("empty id should be a no-op, got err = %v", err)
	}
}

func TestRevokeToken(t *testing.T) {
	s := newTestStore(t)
	uid := int64(11)
	insertSampleToken(t, s, Token{
		ID: "tok-rev-1", DeviceLabel: "laptop",
		GitHubUserID: &uid, GitHubUserLogin: "bob",
	})

	now := time.Now().Unix()
	if err := s.RevokeToken("tok-rev-1", now); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	rows, _ := s.ListTokensForUser(uid)
	if len(rows) != 1 || rows[0].RevokedAt == nil || *rows[0].RevokedAt != now {
		t.Errorf("RevokedAt not set: rows=%+v", rows)
	}

	// Idempotent: revoking again is a no-op (revoked_at IS NULL guard means
	// no second write); the field stays at the first revoke timestamp.
	if err := s.RevokeToken("tok-rev-1", now+100); err != nil {
		t.Fatalf("second RevokeToken: %v", err)
	}
	rows2, _ := s.ListTokensForUser(uid)
	if *rows2[0].RevokedAt != now {
		t.Errorf("revoked_at changed on second revoke: %d, want %d", *rows2[0].RevokedAt, now)
	}
}

func TestRevokeToken_RequiresID(t *testing.T) {
	s := newTestStore(t)
	err := s.RevokeToken("", time.Now().Unix())
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Errorf("want id required error, got %v", err)
	}
}

func TestRevokeToken_UnknownIDIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.RevokeToken("nope", time.Now().Unix()); err != nil {
		t.Errorf("unknown id should be silently idempotent, got err = %v", err)
	}
}

func TestListTokensForUser_OrderingAndEmpty(t *testing.T) {
	s := newTestStore(t)
	uid := int64(42)
	other := int64(99)

	// Older token first, then newer — descending order should put newer first.
	insertSampleToken(t, s, Token{
		ID: "tok-old", DeviceLabel: "old-device",
		GitHubUserID: &uid, GitHubUserLogin: "carol",
		CreatedAt: 1_700_000_000,
	})
	insertSampleToken(t, s, Token{
		ID: "tok-new", DeviceLabel: "new-device", InstallationID: "inst2",
		GitHubUserID: &uid, GitHubUserLogin: "carol",
		CreatedAt: 1_700_000_100,
	})
	// Sibling user must not leak.
	insertSampleToken(t, s, Token{
		ID: "tok-sibling", DeviceLabel: "other-device",
		GitHubUserID: &other, GitHubUserLogin: "dave",
	})

	rows, err := s.ListTokensForUser(uid)
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d tokens, want 2", len(rows))
	}
	if rows[0].ID != "tok-new" || rows[1].ID != "tok-old" {
		t.Errorf("ordering wrong: %s, %s (want tok-new, tok-old)", rows[0].ID, rows[1].ID)
	}
	if rows[0].GitHubUserID == nil || *rows[0].GitHubUserID != uid {
		t.Errorf("GitHubUserID not round-tripped on listing")
	}

	// No rows for an unknown user.
	none, err := s.ListTokensForUser(0)
	if err != nil {
		t.Fatalf("ListTokensForUser(0): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("user 0 should have 0 tokens, got %d", len(none))
	}
}

func TestUpdateInstallationAppSlug_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertInstallation("inst-slug-1", "acme"); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := s.UpdateInstallationAppSlug("inst-slug-1", "caw-acme"); err != nil {
		t.Fatalf("UpdateInstallationAppSlug: %v", err)
	}
	// Round-trip: read via raw query since there's no public getter yet.
	var got string
	if err := s.db.QueryRow(
		`SELECT app_slug FROM installations WHERE installation_id = ?`, "inst-slug-1",
	).Scan(&got); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if got != "caw-acme" {
		t.Errorf("app_slug = %q, want caw-acme", got)
	}
}

func TestUpdateInstallationAppSlug_RequiresInstallationID(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateInstallationAppSlug("", "anything")
	if err == nil || !strings.Contains(err.Error(), "installation_id is required") {
		t.Errorf("want installation_id required error, got %v", err)
	}
}

func TestUpdateInstallationAppSlug_BlankSlugIsAllowed(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertInstallation("inst-blank-slug", "acme"); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	// Blank slug is allowed (no-op-equivalent write); should not error.
	if err := s.UpdateInstallationAppSlug("inst-blank-slug", ""); err != nil {
		t.Errorf("blank slug should be allowed, got err = %v", err)
	}
}

func TestInsertTokenRow_RequiresHash(t *testing.T) {
	s := newTestStore(t)
	err := s.InsertTokenRow(Token{InstallationID: "inst1", Org: "org1"})
	if err == nil || !strings.Contains(err.Error(), "Hash is required") {
		t.Errorf("want Hash required error, got %v", err)
	}
}

func TestInsertTokenRow_RequiresInstallationID(t *testing.T) {
	s := newTestStore(t)
	err := s.InsertTokenRow(Token{Hash: "h-no-inst", Org: "org1"})
	if err == nil || !strings.Contains(err.Error(), "InstallationID is required") {
		t.Errorf("want InstallationID required error, got %v", err)
	}
}

func TestInsertTokenRow_DefaultsDeviceLabelAndCreatedAt(t *testing.T) {
	s := newTestStore(t)
	tok := Token{
		Hash: "hash-defaults", InstallationID: "inst1", Org: "org1",
		// DeviceLabel and CreatedAt deliberately left zero — they must default.
	}
	if err := s.InsertTokenRow(tok); err != nil {
		t.Fatalf("InsertTokenRow: %v", err)
	}
	// Read it back via VerifyToken.
	got, ok, err := s.VerifyToken("hash-defaults")
	if err != nil || !ok {
		t.Fatalf("VerifyToken: ok=%v err=%v", ok, err)
	}
	if got.DeviceLabel != "legacy" {
		t.Errorf("DeviceLabel = %q, want legacy (default)", got.DeviceLabel)
	}
	if got.CreatedAt == 0 {
		t.Errorf("CreatedAt should have been set to now, got 0")
	}
}

func TestInsertTokenRow_ConflictUpdatesAllColumns(t *testing.T) {
	s := newTestStore(t)
	uid1, uid2 := int64(1), int64(2)
	first := Token{
		ID: "tok-conflict", Hash: "h-conflict",
		InstallationID: "inst1", Org: "org1",
		DeviceLabel: "first-device", GitHubUserID: &uid1, GitHubUserLogin: "first",
	}
	if err := s.InsertTokenRow(first); err != nil {
		t.Fatalf("first InsertTokenRow: %v", err)
	}
	// Same hash, different everything else — ON CONFLICT must overwrite.
	second := Token{
		ID: "tok-conflict-2", Hash: "h-conflict",
		InstallationID: "inst2", Org: "org2",
		DeviceLabel: "second-device", GitHubUserID: &uid2, GitHubUserLogin: "second",
	}
	if err := s.InsertTokenRow(second); err != nil {
		t.Fatalf("second InsertTokenRow: %v", err)
	}
	got, ok, err := s.VerifyToken("h-conflict")
	if err != nil || !ok {
		t.Fatalf("VerifyToken: ok=%v err=%v", ok, err)
	}
	if got.InstallationID != "inst2" || got.Org != "org2" || got.DeviceLabel != "second-device" {
		t.Errorf("conflict did not overwrite all columns: %+v", got)
	}
	if got.GitHubUserID == nil || *got.GitHubUserID != uid2 {
		t.Errorf("github_user_id not overwritten: %v", got.GitHubUserID)
	}
}
