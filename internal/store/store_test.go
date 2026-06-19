package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openLegacyV01 opens a fresh SQLite file and applies the pre-Auth-v2 ("v0.1.x")
// shape of the tables that Auth v2 widens: tokens (no id / user / device /
// expiry / revoke columns) and installations (no app_slug). Used by the
// schema-migration tests to verify that production Open() then additively
// upgrades the existing rows without data loss.
func openLegacyV01(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	const legacy = `
	CREATE TABLE tokens (
	    token_hash      TEXT    NOT NULL,
	    installation_id TEXT    NOT NULL,
	    org             TEXT    NOT NULL DEFAULT '',
	    created_at      INTEGER NOT NULL,
	    PRIMARY KEY (token_hash)
	);
	CREATE TABLE installations (
	    installation_id TEXT    NOT NULL,
	    account_login   TEXT    NOT NULL,
	    created_at      INTEGER NOT NULL,
	    PRIMARY KEY (installation_id)
	);
	`
	if _, err := db.Exec(legacy); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func TestOpenAppliesSchemaIdempotently(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.db")
	s1, err := Open(p)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s1.Close()
	// Re-opening runs the schema again; IF NOT EXISTS makes this a no-op.
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	_ = s2.Close()
}

func TestOpenBadPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope", "missing", "x.db")); err == nil {
		t.Fatal("expected error opening db in nonexistent directory")
	}
}

func TestSeenDelivery(t *testing.T) {
	s := newTestStore(t)

	isNew, err := s.SeenDelivery("d1", "push")
	if err != nil || !isNew {
		t.Fatalf("first SeenDelivery = (%v, %v), want (true, nil)", isNew, err)
	}
	isNew, err = s.SeenDelivery("d1", "push")
	if err != nil || isNew {
		t.Fatalf("duplicate SeenDelivery = (%v, %v), want (false, nil)", isNew, err)
	}
	isNew, err = s.SeenDelivery("d2", "pull_request")
	if err != nil || !isNew {
		t.Fatalf("new SeenDelivery = (%v, %v), want (true, nil)", isNew, err)
	}
}

func TestRecordRoundAndExists(t *testing.T) {
	s := newTestStore(t)

	exists, err := s.RoundExists("o", "r", 1, "sha1")
	if err != nil || exists {
		t.Fatalf("RoundExists before = (%v, %v), want (false, nil)", exists, err)
	}
	if err := s.RecordRound("o", "r", 1, "sha1"); err != nil {
		t.Fatalf("RecordRound: %v", err)
	}
	// Upsert of the same key must not error or duplicate.
	if err := s.RecordRound("o", "r", 1, "sha1"); err != nil {
		t.Fatalf("RecordRound (upsert): %v", err)
	}
	exists, err = s.RoundExists("o", "r", 1, "sha1")
	if err != nil || !exists {
		t.Fatalf("RoundExists after = (%v, %v), want (true, nil)", exists, err)
	}
	// A different SHA is a different Round.
	exists, _ = s.RoundExists("o", "r", 1, "sha2")
	if exists {
		t.Fatal("RoundExists for different SHA should be false")
	}
}

func TestPendingLatestWinsPerSignalType(t *testing.T) {
	s := newTestStore(t)

	mk := func(sig, sha, summary string) PendingItem {
		return PendingItem{Owner: "o", Repo: "r", Number: 1, SignalType: sig, SHA: sha, PRState: "open", Summary: summary}
	}
	if err := s.UpsertPending(mk("checks", "a", "first")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPending(mk("checks", "b", "second")); err != nil { // replaces "first"
		t.Fatal(err)
	}
	if err := s.UpsertPending(mk("comments", "a", "a comment")); err != nil {
		t.Fatal(err)
	}

	items, err := s.ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (one per signal-type)", len(items))
	}
	byType := map[string]PendingItem{}
	for _, it := range items {
		byType[it.SignalType] = it
	}
	if got := byType["checks"]; got.Summary != "second" || got.SHA != "b" {
		t.Errorf("checks = %+v, want latest (summary=second, sha=b)", got)
	}
	if got := byType["comments"]; got.Summary != "a comment" {
		t.Errorf("comments = %+v", got)
	}
}

func TestListPendingEmpty(t *testing.T) {
	s := newTestStore(t)
	items, err := s.ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d, want 0", len(items))
	}
}

// TestStoreErrorsAfterClose exercises the error return paths: a closed handle
// makes every query fail.
func TestStoreErrorsAfterClose(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := s.SeenDelivery("d", "e"); err == nil {
		t.Error("SeenDelivery should error after close")
	}
	if err := s.RecordRound("o", "r", 1, "s"); err == nil {
		t.Error("RecordRound should error after close")
	}
	if _, err := s.RoundExists("o", "r", 1, "s"); err == nil {
		t.Error("RoundExists should error after close")
	}
	if err := s.UpsertPending(PendingItem{Owner: "o", Repo: "r", Number: 1, SignalType: "checks"}); err == nil {
		t.Error("UpsertPending should error after close")
	}
	if _, err := s.ListPending(); err == nil {
		t.Error("ListPending should error after close")
	}
	if _, err := s.HasDelivery("d"); err == nil {
		t.Error("HasDelivery should error after close")
	}
}

func TestHasDelivery(t *testing.T) {
	s := newTestStore(t)
	if seen, err := s.HasDelivery("d1"); err != nil || seen {
		t.Fatalf("HasDelivery before = (%v, %v), want (false, nil)", seen, err)
	}
	if _, err := s.SeenDelivery("d1", "push"); err != nil {
		t.Fatal(err)
	}
	if seen, err := s.HasDelivery("d1"); err != nil || !seen {
		t.Fatalf("HasDelivery after = (%v, %v), want (true, nil)", seen, err)
	}
}

func TestSignalsAddReplaceAndList(t *testing.T) {
	s := newTestStore(t)
	mk := func(extID, body string) Signal {
		return Signal{Owner: "o", Repo: "r", Number: 1, SHA: "sha", SignalType: "checks", Source: "CI", ExternalID: extID, Body: body}
	}
	for _, sig := range []Signal{mk("e1", "first"), mk("e1", "second"), mk("e2", "other")} {
		if err := s.AddSignal(sig); err != nil {
			t.Fatalf("AddSignal: %v", err)
		}
	}
	sigs, err := s.SignalsForRound("o", "r", 1, "sha")
	if err != nil {
		t.Fatalf("SignalsForRound: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("got %d signals, want 2 (e1 replaced)", len(sigs))
	}
	for _, sig := range sigs {
		if sig.ExternalID == "e1" && sig.Body != "second" {
			t.Fatalf("e1 not replaced: body = %q", sig.Body)
		}
	}
	if other, _ := s.SignalsForRound("o", "r", 1, "different"); len(other) != 0 {
		t.Fatalf("other SHA should have no signals, got %d", len(other))
	}
}

func TestLatestRoundSHA(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.LatestRoundSHA("o", "r", 1); err != nil || ok {
		t.Fatalf("no rounds yet: ok=%v err=%v, want false/nil", ok, err)
	}
	if err := s.RecordRound("o", "r", 1, "sha1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRound("o", "r", 1, "sha2"); err != nil {
		t.Fatal(err)
	}
	sha, ok, err := s.LatestRoundSHA("o", "r", 1)
	if err != nil || !ok || sha != "sha2" {
		t.Fatalf("LatestRoundSHA = (%q, %v, %v), want (sha2, true, nil)", sha, ok, err)
	}
}

func TestTokensInsertVerifyUpsert(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertToken("hashA", "inst1", "org1"); err != nil {
		t.Fatal(err)
	}
	tok, ok, err := s.VerifyToken("hashA")
	if err != nil || !ok || tok.InstallationID != "inst1" {
		t.Fatalf("VerifyToken known = (%+v, %v, %v), want InstallationID=inst1, ok=true", tok, ok, err)
	}
	if tok.GitHubUserID != nil {
		t.Errorf("legacy token GitHubUserID = %v, want nil", *tok.GitHubUserID)
	}
	if tok.DeviceLabel != "legacy" {
		t.Errorf("legacy DeviceLabel = %q, want legacy", tok.DeviceLabel)
	}
	if tok.ID == "" {
		t.Error("ID should be backfilled (legacy-<rowid>); got empty")
	}
	// Second VerifyToken should observe the persisted backfill rather than
	// re-deriving (idempotent).
	tok2, _, _ := s.VerifyToken("hashA")
	if tok2.ID != tok.ID {
		t.Errorf("backfill not idempotent: %q != %q", tok2.ID, tok.ID)
	}

	if _, ok, err := s.VerifyToken("unknown"); err != nil || ok {
		t.Fatalf("VerifyToken unknown = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if err := s.InsertToken("hashA", "inst2", ""); err != nil { // upsert
		t.Fatal(err)
	}
	if tok, _, _ := s.VerifyToken("hashA"); tok.InstallationID != "inst2" {
		t.Fatalf("after upsert InstallationID = %q, want inst2", tok.InstallationID)
	}
}

// TestTokensInsertRowFullShape exercises the Auth v2 InsertTokenRow surface:
// user-bound row round-trips through VerifyToken with GitHubUserID / Login /
// DeviceLabel / ExpiresAt set; revoked / expired rows are filtered out.
func TestTokensInsertRowFullShape(t *testing.T) {
	s := newTestStore(t)
	uid := int64(12345)
	exp := int64(9_999_999_999) // far future
	row := Token{
		ID:              "01HX0000000000000000000001",
		Hash:            "user-bound-hash",
		InstallationID:  "inst-1",
		Org:             "ravencloak-org",
		GitHubUserID:    &uid,
		GitHubUserLogin: "octocat",
		DeviceLabel:     "Claude Code @ jobin-mbp",
		CreatedAt:       1_700_000_000,
		ExpiresAt:       &exp,
	}
	if err := s.InsertTokenRow(row); err != nil {
		t.Fatalf("InsertTokenRow: %v", err)
	}
	got, ok, err := s.VerifyToken("user-bound-hash")
	if err != nil || !ok {
		t.Fatalf("VerifyToken: (ok=%v, err=%v)", ok, err)
	}
	if got.ID != row.ID {
		t.Errorf("ID = %q, want %q", got.ID, row.ID)
	}
	if got.GitHubUserID == nil || *got.GitHubUserID != uid {
		t.Errorf("GitHubUserID = %v, want %d", got.GitHubUserID, uid)
	}
	if got.GitHubUserLogin != "octocat" {
		t.Errorf("GitHubUserLogin = %q, want octocat", got.GitHubUserLogin)
	}
	if got.DeviceLabel != "Claude Code @ jobin-mbp" {
		t.Errorf("DeviceLabel = %q", got.DeviceLabel)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Errorf("ExpiresAt = %v, want %d", got.ExpiresAt, exp)
	}

	// Revoke filters from VerifyToken.
	if err := s.RevokeToken(row.ID, 1_700_000_500); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, ok, _ := s.VerifyToken("user-bound-hash"); ok {
		t.Fatal("revoked token still verifies")
	}

	// Expired row: separate hash.
	pastExp := int64(1) // long expired
	expired := row
	expired.ID = "01HX0000000000000000000002"
	expired.Hash = "expired-hash"
	expired.ExpiresAt = &pastExp
	if err := s.InsertTokenRow(expired); err != nil {
		t.Fatalf("InsertTokenRow expired: %v", err)
	}
	if _, ok, _ := s.VerifyToken("expired-hash"); ok {
		t.Fatal("expired token still verifies")
	}
}

// TestTokensListForUser groups token rows by github_user_id, including revoked
// rows (so the management UI can show them).
func TestTokensListForUser(t *testing.T) {
	s := newTestStore(t)
	uid := int64(42)
	for i, h := range []string{"a", "b", "c"} {
		if err := s.InsertTokenRow(Token{
			ID:             string(rune('A'+i)) + "0000000000000000000000000",
			Hash:           h,
			InstallationID: "inst-1",
			GitHubUserID:   &uid,
			DeviceLabel:    "dev",
			CreatedAt:      int64(1_700_000_000 + i),
		}); err != nil {
			t.Fatalf("InsertTokenRow %s: %v", h, err)
		}
	}
	other := int64(99)
	if err := s.InsertTokenRow(Token{
		ID:             "Z0000000000000000000000000",
		Hash:           "other",
		InstallationID: "inst-1",
		GitHubUserID:   &other,
		DeviceLabel:    "dev",
		CreatedAt:      1_700_000_010,
	}); err != nil {
		t.Fatalf("InsertTokenRow other: %v", err)
	}
	rows, err := s.ListTokensForUser(uid)
	if err != nil {
		t.Fatalf("ListTokensForUser: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.GitHubUserID == nil || *r.GitHubUserID != uid {
			t.Errorf("row %q user_id = %v, want %d", r.Hash, r.GitHubUserID, uid)
		}
	}
	// Different user must not bleed.
	if rows, _ := s.ListTokensForUser(int64(99)); len(rows) != 1 {
		t.Errorf("other user rows = %d, want 1", len(rows))
	}
}

// TestTokensByGitHubUserID_FiltersActive proves the auth-v2 Phase 3.5
// webhook fan-out lookup returns only active rows (not revoked, not expired)
// for the requested user — revoked / expired devices must NOT receive
// pr_opened events.
func TestTokensByGitHubUserID_FiltersActive(t *testing.T) {
	s := newTestStore(t)
	uid := int64(42)
	now := int64(1_700_000_100)
	live := int64(1_700_001_000)
	past := int64(1_700_000_050)
	revokedAt := int64(1_700_000_080)

	// Active row — should be returned.
	if err := s.InsertTokenRow(Token{
		ID:             "A0000000000000000000000000",
		Hash:           "active",
		InstallationID: "inst-1",
		GitHubUserID:   &uid,
		DeviceLabel:    "dev-1",
		CreatedAt:      1_700_000_000,
		ExpiresAt:      &live,
	}); err != nil {
		t.Fatalf("InsertTokenRow active: %v", err)
	}
	// Revoked row — filtered.
	if err := s.InsertTokenRow(Token{
		ID:             "B0000000000000000000000000",
		Hash:           "revoked",
		InstallationID: "inst-1",
		GitHubUserID:   &uid,
		DeviceLabel:    "dev-2",
		CreatedAt:      1_700_000_000,
		ExpiresAt:      &live,
		RevokedAt:      &revokedAt,
	}); err != nil {
		t.Fatalf("InsertTokenRow revoked: %v", err)
	}
	// Expired row — filtered.
	if err := s.InsertTokenRow(Token{
		ID:             "C0000000000000000000000000",
		Hash:           "expired",
		InstallationID: "inst-1",
		GitHubUserID:   &uid,
		DeviceLabel:    "dev-3",
		CreatedAt:      1_700_000_000,
		ExpiresAt:      &past,
	}); err != nil {
		t.Fatalf("InsertTokenRow expired: %v", err)
	}
	// Different user — filtered (cross-user isolation).
	other := int64(99)
	if err := s.InsertTokenRow(Token{
		ID:             "D0000000000000000000000000",
		Hash:           "other",
		InstallationID: "inst-1",
		GitHubUserID:   &other,
		DeviceLabel:    "dev-4",
		CreatedAt:      1_700_000_000,
	}); err != nil {
		t.Fatalf("InsertTokenRow other: %v", err)
	}

	rows, err := s.TokensByGitHubUserID(uid, now)
	if err != nil {
		t.Fatalf("TokensByGitHubUserID: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 active", len(rows))
	}
	if rows[0].Hash != "active" {
		t.Errorf("hash = %q, want active", rows[0].Hash)
	}

	// userID 0 is a sentinel for "legacy / unbound" and MUST return empty.
	if rows, err := s.TokensByGitHubUserID(0, now); err != nil || len(rows) != 0 {
		t.Errorf("TokensByGitHubUserID(0) = (%d rows, err=%v), want (0, nil)", len(rows), err)
	}
	// Unknown user → empty slice, no error.
	if rows, err := s.TokensByGitHubUserID(int64(7777), now); err != nil || len(rows) != 0 {
		t.Errorf("TokensByGitHubUserID unknown = (%d rows, err=%v)", len(rows), err)
	}
}

// TestTokensForInstallation returns active tokens scoped to one installation;
// auth-v2 Phase 3.5 uses it to find users with an active credential for an
// installation when installation_repositories.added fires.
func TestTokensForInstallation(t *testing.T) {
	s := newTestStore(t)
	uid := int64(42)
	now := int64(1_700_000_100)
	live := int64(1_700_001_000)

	if err := s.InsertTokenRow(Token{
		ID:             "A0000000000000000000000000",
		Hash:           "instA-1",
		InstallationID: "inst-A",
		GitHubUserID:   &uid,
		DeviceLabel:    "dev",
		CreatedAt:      1_700_000_000,
		ExpiresAt:      &live,
	}); err != nil {
		t.Fatalf("InsertTokenRow: %v", err)
	}
	if err := s.InsertTokenRow(Token{
		ID:             "B0000000000000000000000000",
		Hash:           "instB-1",
		InstallationID: "inst-B",
		GitHubUserID:   &uid,
		DeviceLabel:    "dev",
		CreatedAt:      1_700_000_000,
		ExpiresAt:      &live,
	}); err != nil {
		t.Fatalf("InsertTokenRow: %v", err)
	}

	rows, err := s.TokensForInstallation("inst-A", now)
	if err != nil {
		t.Fatalf("TokensForInstallation: %v", err)
	}
	if len(rows) != 1 || rows[0].InstallationID != "inst-A" {
		t.Fatalf("rows = %+v, want one inst-A row", rows)
	}
}

// TestUpdateInstallationAppSlug updates a previously-upserted installation.
func TestUpdateInstallationAppSlug(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertInstallation("inst-1", "ravencloak-org"); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := s.UpdateInstallationAppSlug("inst-1", "caw-ravencloak"); err != nil {
		t.Fatalf("UpdateInstallationAppSlug: %v", err)
	}
	var slug string
	if err := s.db.QueryRow(`SELECT app_slug FROM installations WHERE installation_id = ?`, "inst-1").Scan(&slug); err != nil {
		t.Fatalf("read app_slug: %v", err)
	}
	if slug != "caw-ravencloak" {
		t.Errorf("app_slug = %q, want caw-ravencloak", slug)
	}
	// Missing id → no error (idempotent), just zero rows.
	if err := s.UpdateInstallationAppSlug("nope", "x"); err != nil {
		t.Errorf("UpdateInstallationAppSlug missing id: %v", err)
	}
}

// TestSchemaMigratesV0_1Database simulates a v0.1.x database (legacy tokens
// schema, no Auth v2 columns) and verifies Open() additively migrates it
// online without touching the existing rows.
func TestSchemaMigratesV0_1Database(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v01.db")

	// Bootstrap the legacy schema by hand (a slim subset that mirrors the
	// production v0.1 shape: no id / github_user_id / device_label / ... ).
	legacy, err := openLegacyV01(p)
	if err != nil {
		t.Fatalf("openLegacyV01: %v", err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO tokens (token_hash, installation_id, org, created_at) VALUES (?, ?, ?, ?)`,
		"legacy-hash", "inst-old", "old-org", int64(1_600_000_000),
	); err != nil {
		t.Fatalf("seed legacy token: %v", err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO installations (installation_id, account_login, created_at) VALUES (?, ?, ?)`,
		"inst-old", "old-org", int64(1_600_000_000),
	); err != nil {
		t.Fatalf("seed legacy installation: %v", err)
	}
	_ = legacy.Close()

	// Re-open through the production path — this applies the new schema and
	// the additive migrations on the existing tables.
	s, err := Open(p)
	if err != nil {
		t.Fatalf("Open existing v0.1 db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Legacy row still verifies; backfill populates ID; GitHubUserID stays nil.
	tok, ok, err := s.VerifyToken("legacy-hash")
	if err != nil || !ok {
		t.Fatalf("VerifyToken after migration: (ok=%v, err=%v)", ok, err)
	}
	if tok.InstallationID != "inst-old" {
		t.Errorf("InstallationID = %q, want inst-old", tok.InstallationID)
	}
	if tok.GitHubUserID != nil {
		t.Errorf("legacy GitHubUserID = %v, want nil", *tok.GitHubUserID)
	}
	if tok.ID == "" {
		t.Error("backfilled ID should be non-empty")
	}
	// New column app_slug defaults to '' on the migrated installations row.
	var slug string
	if err := s.db.QueryRow(`SELECT app_slug FROM installations WHERE installation_id = ?`, "inst-old").Scan(&slug); err != nil {
		t.Fatalf("read app_slug post-migration: %v", err)
	}
	if slug != "" {
		t.Errorf("app_slug default = %q, want \"\"", slug)
	}

	// auth_sessions is freshly created; CRUD round-trips.
	if err := s.InsertAuthSession(AuthSession{
		ID:                  "01HXSESSIONIDSAMPLE000A001",
		HandshakeMode:       "loopback",
		CodeChallenge:       "abc",
		CodeChallengeMethod: "S256",
		ClientLabel:         "test",
		CreatedAt:           1_700_000_000,
		ExpiresAt:           1_700_000_600,
	}); err != nil {
		t.Fatalf("InsertAuthSession on migrated db: %v", err)
	}
	if _, ok, err := s.GetAuthSession("01HXSESSIONIDSAMPLE000A001"); err != nil || !ok {
		t.Fatalf("GetAuthSession: (ok=%v, err=%v)", ok, err)
	}

	// Re-open once more — migrations idempotent (duplicate column name path).
	_ = s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	_ = s2.Close()
}

func TestInstallationForRepo(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddInstallationRepo("42", "ravencloak-org/caw"); err != nil {
		t.Fatalf("AddInstallationRepo: %v", err)
	}
	id, ok, err := s.InstallationForRepo("ravencloak-org/caw")
	if err != nil || !ok || id != "42" {
		t.Fatalf("InstallationForRepo = (%q,%v,%v), want (42,true,nil)", id, ok, err)
	}
	if _, ok, err := s.InstallationForRepo("ravencloak-org/unknown"); err != nil || ok {
		t.Fatalf("unknown repo: ok=%v err=%v, want (false,nil)", ok, err)
	}
}

func TestInstallationForRepoStoreError(t *testing.T) {
	s := newTestStore(t)
	_ = s.Close() // a closed store errors on query (not sql.ErrNoRows)
	if _, _, err := s.InstallationForRepo("o/r"); err == nil {
		t.Fatal("expected error querying a closed store")
	}
}
