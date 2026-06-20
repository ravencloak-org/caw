package hub

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/ravencloak-org/caw/internal/github"
	"github.com/ravencloak-org/caw/internal/store"
)

// openTestStore opens an in-memory SQLite store for testing.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	_ = f.Close()
	st, err := store.Open(f.Name())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// installEnvelope builds a github.Envelope for an installation event.
func installEnvelope(action string, id int64, org string, rs []github.Repository) github.Envelope {
	inst := &github.Installation{ID: id}
	inst.Account.Login = org
	return github.Envelope{
		Action:       action,
		Installation: inst,
		Repositories: rs,
	}
}

// installReposEnvelope builds a github.Envelope for an installation_repositories event.
func installReposEnvelope(id int64, added, removed []github.Repository) github.Envelope {
	return github.Envelope{
		Action: "added",
		Installation: &github.Installation{
			ID: id,
		},
		RepositoriesAdded:   added,
		RepositoriesRemoved: removed,
	}
}

func repos(names ...string) []github.Repository {
	rs := make([]github.Repository, len(names))
	for i, n := range names {
		rs[i] = github.Repository{FullName: n}
	}
	return rs
}

// TestHandleInstallation_Created checks that a "created" event upserts the
// installation and its repositories.
func TestHandleInstallation_Created(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)

	env := installEnvelope("created", 42, "acme", repos("acme/alpha", "acme/beta"))
	if err := h.handleInstallation(env); err != nil {
		t.Fatalf("handleInstallation: %v", err)
	}

	for _, fullName := range []string{"acme/alpha", "acme/beta"} {
		ok, err := st.RepoInInstallation("42", fullName)
		if err != nil {
			t.Fatalf("RepoInInstallation(%s): %v", fullName, err)
		}
		if !ok {
			t.Errorf("expected %s to be in installation 42", fullName)
		}
	}
}

// TestHandleInstallation_Created_CallsMintFn checks that mintFn is invoked on creation.
func TestHandleInstallation_Created_CallsMintFn(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)

	called := false
	var gotID, gotOrg, gotLabel string
	h.WithMintFunc(MintFunc(func(installationID, org, deviceLabel string, _ int64, _ string) (string, string, error) {
		called = true
		gotID = installationID
		gotOrg = org
		gotLabel = deviceLabel
		return "raw-token", "01HXWEBHOOKMINT0000000000", nil
	}))

	env := installEnvelope("created", 99, "org99", nil)
	if err := h.handleInstallation(env); err != nil {
		t.Fatalf("handleInstallation: %v", err)
	}

	if !called {
		t.Fatal("mintFn was not called")
	}
	if gotID != "99" {
		t.Errorf("mintFn installationID = %q, want %q", gotID, "99")
	}
	if gotOrg != "org99" {
		t.Errorf("mintFn org = %q, want %q", gotOrg, "org99")
	}
	if gotLabel != "installation-auto" {
		t.Errorf("mintFn deviceLabel = %q, want \"installation-auto\" (Phase 1 webhook auto-mint)", gotLabel)
	}
}

// TestHandleInstallation_Created_MintFnError checks that a mintFn error is non-fatal.
func TestHandleInstallation_Created_MintFnError(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)
	h.WithMintFunc(MintFunc(func(_, _, _ string, _ int64, _ string) (string, string, error) {
		return "", "", fmt.Errorf("mint failed")
	}))

	env := installEnvelope("created", 7, "org7", nil)
	// Must not return an error even though mintFn fails.
	if err := h.handleInstallation(env); err != nil {
		t.Fatalf("handleInstallation should not propagate mint error, got: %v", err)
	}
}

// TestHandleInstallation_Deleted checks that the installation is removed.
func TestHandleInstallation_Deleted(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)

	// First create, then delete.
	const id int64 = 55
	idStr := strconv.FormatInt(id, 10)

	envCreate := installEnvelope("created", id, "org55", repos("org55/repo"))
	if err := h.handleInstallation(envCreate); err != nil {
		t.Fatalf("handleInstallation created: %v", err)
	}

	ok, err := st.RepoInInstallation(idStr, "org55/repo")
	if err != nil || !ok {
		t.Fatalf("pre-delete: repo should exist; ok=%v err=%v", ok, err)
	}

	envDelete := installEnvelope("deleted", id, "org55", nil)
	if err := h.handleInstallation(envDelete); err != nil {
		t.Fatalf("handleInstallation deleted: %v", err)
	}

	ok, err = st.RepoInInstallation(idStr, "org55/repo")
	if err != nil {
		t.Fatalf("post-delete RepoInInstallation: %v", err)
	}
	if ok {
		t.Error("repo should not exist after installation delete")
	}
}

// TestHandleInstallation_DeletedRevokesTokens covers Auth v2 Phase 4's
// webhook-driven revocation: a deleted installation kills every still-active
// token bound to it, plus any cached repo-access decisions.
func TestHandleInstallation_DeletedRevokesTokens(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)

	const id int64 = 77
	idStr := strconv.FormatInt(id, 10)
	uid := int64(42)

	envCreate := installEnvelope("created", id, "org77", repos("org77/repo"))
	if err := h.handleInstallation(envCreate); err != nil {
		t.Fatalf("handleInstallation created: %v", err)
	}
	// Seed three tokens bound to this installation (one of them already
	// revoked, so we can confirm the handler doesn't blindly overwrite the
	// original revoke timestamp).
	preRev := int64(1_600_000_000)
	rows := []store.Token{
		{ID: "A0000000000000000000000001", Hash: "h-a", InstallationID: idStr, Org: "org77", GitHubUserID: &uid, DeviceLabel: "dev", CreatedAt: 1_700_000_000},
		{ID: "A0000000000000000000000002", Hash: "h-b", InstallationID: idStr, Org: "org77", GitHubUserID: &uid, DeviceLabel: "dev", CreatedAt: 1_700_000_001},
		{ID: "A0000000000000000000000003", Hash: "h-c", InstallationID: idStr, Org: "org77", GitHubUserID: &uid, DeviceLabel: "dev", CreatedAt: 1_700_000_002, RevokedAt: &preRev},
	}
	// One token bound to a different installation must NOT be revoked.
	other := store.Token{
		ID: "B0000000000000000000000001", Hash: "h-other", InstallationID: "999",
		Org: "elsewhere", GitHubUserID: &uid, DeviceLabel: "dev", CreatedAt: 1_700_000_003,
	}
	for _, r := range rows {
		if err := st.InsertTokenRow(r); err != nil {
			t.Fatalf("InsertTokenRow %s: %v", r.Hash, err)
		}
	}
	if err := st.InsertTokenRow(other); err != nil {
		t.Fatalf("InsertTokenRow other: %v", err)
	}

	envDelete := installEnvelope("deleted", id, "org77", nil)
	if err := h.handleInstallation(envDelete); err != nil {
		t.Fatalf("handleInstallation deleted: %v", err)
	}

	// Every active row of installation 77 is now revoked.
	for _, id := range []string{"A0000000000000000000000001", "A0000000000000000000000002"} {
		got, ok, err := st.GetTokenByID(id)
		if err != nil || !ok {
			t.Fatalf("GetTokenByID %s: ok=%v err=%v", id, ok, err)
		}
		if got.RevokedAt == nil {
			t.Errorf("row %s: revoked_at still nil after installation delete", id)
		}
	}
	// Pre-revoked row keeps its original timestamp (the UPDATE only fires
	// when revoked_at IS NULL; the audit-of-record is the first revoke).
	got, _, _ := st.GetTokenByID("A0000000000000000000000003")
	if got.RevokedAt == nil || *got.RevokedAt != preRev {
		t.Errorf("pre-revoked row revoked_at = %v, want preserved %d", got.RevokedAt, preRev)
	}
	// Other installation untouched.
	got, _, _ = st.GetTokenByID("B0000000000000000000000001")
	if got.RevokedAt != nil {
		t.Errorf("other-installation token revoked: %v", got.RevokedAt)
	}
}

// TestHandleInstallation_NilInstallation checks that a nil Installation is a no-op.
func TestHandleInstallation_NilInstallation(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)
	env := github.Envelope{Action: "created"}
	if err := h.handleInstallation(env); err != nil {
		t.Fatalf("handleInstallation with nil installation: %v", err)
	}
}

// TestHandleInstallation_UnknownAction checks that an unknown action is a no-op.
func TestHandleInstallation_UnknownAction(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)
	env := installEnvelope("suspended", 3, "org3", nil)
	if err := h.handleInstallation(env); err != nil {
		t.Fatalf("handleInstallation with unknown action: %v", err)
	}
}

// TestHandleInstallationRepositories_AddedAndRemoved checks that repos are
// correctly added and removed when the event fires.
func TestHandleInstallationRepositories_AddedAndRemoved(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)
	const id int64 = 10
	idStr := "10"

	// Bootstrap the installation.
	if err := st.UpsertInstallation(idStr, "org10"); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := st.AddInstallationRepo(idStr, "org10/keep"); err != nil {
		t.Fatalf("AddInstallationRepo: %v", err)
	}
	if err := st.AddInstallationRepo(idStr, "org10/remove"); err != nil {
		t.Fatalf("AddInstallationRepo: %v", err)
	}

	env := installReposEnvelope(id, repos("org10/new"), repos("org10/remove"))
	if err := h.handleInstallationRepositories(env); err != nil {
		t.Fatalf("handleInstallationRepositories: %v", err)
	}

	for _, tc := range []struct {
		fullName string
		want     bool
	}{
		{"org10/keep", true},
		{"org10/new", true},
		{"org10/remove", false},
	} {
		ok, err := st.RepoInInstallation(idStr, tc.fullName)
		if err != nil {
			t.Fatalf("RepoInInstallation(%s): %v", tc.fullName, err)
		}
		if ok != tc.want {
			t.Errorf("RepoInInstallation(%s) = %v, want %v", tc.fullName, ok, tc.want)
		}
	}
}

// TestHandleInstallationRepositories_NilInstallation checks nil is a no-op.
func TestHandleInstallationRepositories_NilInstallation(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)
	env := github.Envelope{}
	if err := h.handleInstallationRepositories(env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestIngest_InstallationRouting checks that ingest routes installation events
// before the owner/repo guard.
func TestIngest_InstallationRouting(t *testing.T) {
	st := openTestStore(t)
	h := New(st, nil, nil)

	// An installation event has no repository fields; ingest must not drop it.
	env := installEnvelope("created", 20, "org20", nil)
	if err := h.ingest(context.Background(), "installation", env); err != nil {
		t.Fatalf("ingest installation: %v", err)
	}

	env2 := installReposEnvelope(20, repos("org20/foo"), nil)
	if err := h.ingest(context.Background(), "installation_repositories", env2); err != nil {
		t.Fatalf("ingest installation_repositories: %v", err)
	}
	ok, err := st.RepoInInstallation("20", "org20/foo")
	if err != nil {
		t.Fatalf("RepoInInstallation: %v", err)
	}
	if !ok {
		t.Error("org20/foo should be in installation 20 after ingest")
	}
}
