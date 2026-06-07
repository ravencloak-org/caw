package store

import (
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
	id, ok, err := s.VerifyToken("hashA")
	if err != nil || !ok || id != "inst1" {
		t.Fatalf("VerifyToken known = (%q, %v, %v), want (inst1, true, nil)", id, ok, err)
	}
	if _, ok, err := s.VerifyToken("unknown"); err != nil || ok {
		t.Fatalf("VerifyToken unknown = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if err := s.InsertToken("hashA", "inst2", ""); err != nil { // upsert
		t.Fatal(err)
	}
	if id, _, _ := s.VerifyToken("hashA"); id != "inst2" {
		t.Fatalf("after upsert id = %q, want inst2", id)
	}
}
