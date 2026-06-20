package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravencloak-org/caw/internal/store"
)

// TestMigrateTokens_RevokesActiveLegacyRows: the Auth v2 Phase 5 operator
// subcommand walks every active legacy row and revokes it. Output names each
// affected (token_id, installation_id, org) tuple so a wrapping shell
// pipeline can grep for confirmation, then a final "Revoked N legacy
// tokens" summary line.
func TestMigrateTokens_RevokesActiveLegacyRows(t *testing.T) {
	st := openTestStore(t)
	uid := int64(42)

	// Two active legacy rows + one revoked + one user-bound — only the two
	// active legacy rows should be touched.
	insertLegacy(t, st, "L00000000000000000000000001", "leg-a", "inst-1", "org-1")
	insertLegacy(t, st, "L00000000000000000000000002", "leg-b", "inst-2", "org-2")
	// Already revoked legacy: must NOT be re-touched (audit timestamp stays).
	revokedAt := int64(1)
	if err := st.InsertTokenRow(store.Token{
		ID:             "L00000000000000000000000003",
		Hash:           "leg-rev",
		InstallationID: "inst-1",
		Org:            "org-1",
		DeviceLabel:    "legacy",
		RevokedAt:      &revokedAt,
	}); err != nil {
		t.Fatalf("insert revoked legacy: %v", err)
	}
	// User-bound: must NOT be touched.
	if err := st.InsertTokenRow(store.Token{
		ID:              "U00000000000000000000000001",
		Hash:            "user-1",
		InstallationID:  "inst-1",
		Org:             "org-1",
		GitHubUserID:    &uid,
		GitHubUserLogin: "alice",
		DeviceLabel:     "dev",
	}); err != nil {
		t.Fatalf("insert user-bound: %v", err)
	}

	var buf bytes.Buffer
	if err := migrateTokens(st, nil, &buf); err != nil {
		t.Fatalf("migrateTokens: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"token_id=L00000000000000000000000001",
		"token_id=L00000000000000000000000002",
		"installation_id=inst-1",
		"installation_id=inst-2",
		"org=org-1",
		"org=org-2",
		"Revoked 2 legacy tokens",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// User-bound row id must NOT appear in the output.
	if strings.Contains(out, "U00000000000000000000000001") {
		t.Errorf("user-bound row leaked into output:\n%s", out)
	}

	// Both active legacy rows are now revoked: a second invocation prints
	// the zero-row steady-state summary and exits success.
	buf.Reset()
	if err := migrateTokens(st, nil, &buf); err != nil {
		t.Fatalf("migrateTokens idempotent: %v", err)
	}
	if !strings.Contains(buf.String(), "Revoked 0 legacy tokens") {
		t.Errorf("steady-state output missing zero-row summary:\n%s", buf.String())
	}
}

// TestMigrateTokens_DryRunWritesNothingToDB: --dry-run prints the same
// per-row output but does not call RevokeToken. The next invocation finds
// the same legacy rows still active.
func TestMigrateTokens_DryRunWritesNothingToDB(t *testing.T) {
	st := openTestStore(t)
	insertLegacy(t, st, "L00000000000000000000000010", "dry-a", "inst-A", "org-A")
	insertLegacy(t, st, "L00000000000000000000000011", "dry-b", "inst-B", "org-B")

	var buf bytes.Buffer
	if err := migrateTokens(st, []string{"--dry-run"}, &buf); err != nil {
		t.Fatalf("migrateTokens --dry-run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Would revoke 2 legacy tokens") {
		t.Errorf("dry-run summary missing:\n%s", out)
	}
	if !strings.Contains(out, "token_id=L00000000000000000000000010") {
		t.Errorf("dry-run per-row line missing:\n%s", out)
	}

	// Re-list: both rows still active because --dry-run did not write.
	rows, err := st.ListLegacyTokens(0)
	if err != nil {
		t.Fatalf("ListLegacyTokens post dry-run: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("active legacy rows post dry-run = %d, want 2", len(rows))
	}
}

// TestMigrateTokens_UnknownArgRejected: any arg other than --dry-run is a
// usage error so an operator-typo never silently no-ops.
func TestMigrateTokens_UnknownArgRejected(t *testing.T) {
	st := openTestStore(t)
	var buf bytes.Buffer
	err := migrateTokens(st, []string{"--bind-user", "jobinlawrance"}, &buf)
	if err == nil {
		t.Fatal("want error on unknown arg, got nil")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("err = %q, want a 'usage' message", err.Error())
	}
}

// openTestStore is the cmd/hub test bootstrap — mirrors internal/store's
// newTestStore so the operator-subcommand tests open the same shape of DB.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// insertLegacy seeds one active legacy (NULL github_user_id) token row.
func insertLegacy(t *testing.T, st *store.Store, id, hash, installID, org string) {
	t.Helper()
	if err := st.InsertTokenRow(store.Token{
		ID:             id,
		Hash:           hash,
		InstallationID: installID,
		Org:            org,
		DeviceLabel:    "legacy",
	}); err != nil {
		t.Fatalf("insert legacy %s: %v", hash, err)
	}
}
