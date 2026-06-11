package rebase

import (
	"context"
	"errors"
	"testing"

	"github.com/ravencloak-org/caw/internal/store"
)

// --- fakeLeaseStore ---

type fakeLeaseStore struct {
	acquireResult store.AcquireLeaseResult
	acquireErr    error
	renewErr      error
	releaseErr    error
	renewCount    int
	released      bool
}

func (f *fakeLeaseStore) AcquireLease(_, _ string, _ int, _ string, _ int64) (store.AcquireLeaseResult, error) {
	return f.acquireResult, f.acquireErr
}

func (f *fakeLeaseStore) RenewLease(_, _ string, _ int, _ string, _ int64) (store.Lease, error) {
	f.renewCount++
	return store.Lease{}, f.renewErr
}

func (f *fakeLeaseStore) ReleaseLease(_, _ string, _ int, _ string) error {
	f.released = true
	return f.releaseErr
}

// --- fakeAutoMerger ---

type fakeAutoMerger struct {
	mergeErr error
	called   bool
}

func (f *fakeAutoMerger) EnableAutoMerge(_ context.Context, _, _ string, _ int, _ string) error {
	f.called = true
	return f.mergeErr
}

// --- helpers ---

func grantedStore() *fakeLeaseStore {
	return &fakeLeaseStore{
		acquireResult: store.AcquireLeaseResult{
			Granted: true,
			Lease:   store.Lease{Holder: "hub-orphan"},
		},
	}
}

func deniedStore() *fakeLeaseStore {
	return &fakeLeaseStore{
		acquireResult: store.AcquireLeaseResult{
			Granted: false,
			Lease:   store.Lease{Holder: "someone-else"},
		},
	}
}

// --- tests ---

func TestOrphanHandler_TriggerOrphanRebase_Success(t *testing.T) {
	st := grantedStore()
	runner := &fakeGitRunner{}
	merger := &fakeAutoMerger{}

	h := NewOrphanHandler("hub-orphan", st, runner, merger)
	if err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !runner.fetchCalled {
		t.Error("Fetch not called")
	}
	if !runner.rebaseCalled {
		t.Error("Rebase not called")
	}
	if !runner.pushCalled {
		t.Error("ForcePushWithLease not called")
	}
	if !merger.called {
		t.Error("EnableAutoMerge not called after successful rebase")
	}
	if !st.released {
		t.Error("ReleaseLease not called")
	}
}

func TestOrphanHandler_TriggerOrphanRebase_LeaseDenied_Skips(t *testing.T) {
	st := deniedStore()
	runner := &fakeGitRunner{}
	merger := &fakeAutoMerger{}

	h := NewOrphanHandler("hub-orphan", st, runner, merger)
	if err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123"); err != nil {
		t.Fatalf("expected nil on denied lease, got %v", err)
	}
	if runner.fetchCalled || runner.rebaseCalled || runner.pushCalled {
		t.Error("git operations must not run when lease is denied")
	}
	if merger.called {
		t.Error("EnableAutoMerge must not be called when lease is denied")
	}
}

func TestOrphanHandler_TriggerOrphanRebase_AcquireError(t *testing.T) {
	st := &fakeLeaseStore{acquireErr: errors.New("db down")}
	h := NewOrphanHandler("hub-orphan", st, &fakeGitRunner{}, &fakeAutoMerger{})
	if err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestOrphanHandler_TriggerOrphanRebase_FetchError_ReleasesLease(t *testing.T) {
	st := grantedStore()
	runner := &fakeGitRunner{fetchErr: errors.New("fetch fail")}
	merger := &fakeAutoMerger{}

	h := NewOrphanHandler("hub-orphan", st, runner, merger)
	err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !st.released {
		t.Error("ReleaseLease must be called even after fetch error")
	}
	if merger.called {
		t.Error("EnableAutoMerge must not be called after fetch failure")
	}
}

func TestOrphanHandler_TriggerOrphanRebase_RebaseError_ReleasesLease(t *testing.T) {
	st := grantedStore()
	runner := &fakeGitRunner{rebaseErr: errors.New("conflict")}
	merger := &fakeAutoMerger{}

	h := NewOrphanHandler("hub-orphan", st, runner, merger)
	err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !st.released {
		t.Error("ReleaseLease must be called even after rebase error")
	}
	if merger.called {
		t.Error("EnableAutoMerge must not be called after rebase failure")
	}
}

func TestOrphanHandler_TriggerOrphanRebase_AutoMergeError_NonFatal(t *testing.T) {
	// Auto-merge failure must not cause TriggerOrphanRebase to return an error.
	st := grantedStore()
	merger := &fakeAutoMerger{mergeErr: errors.New("auto-merge not enabled")}

	h := NewOrphanHandler("hub-orphan", st, &fakeGitRunner{}, merger)
	if err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123"); err != nil {
		t.Fatalf("auto-merge error must be non-fatal, got: %v", err)
	}
	if !st.released {
		t.Error("ReleaseLease must be called")
	}
}

func TestOrphanHandler_TriggerOrphanRebase_NilMerger_NoAutoMerge(t *testing.T) {
	st := grantedStore()
	h := NewOrphanHandler("hub-orphan", st, &fakeGitRunner{}, nil)
	if err := h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestOrphanHandler_TriggerOrphanRebase_CustomBranchResolver(t *testing.T) {
	st := grantedStore()
	runner := &fakeGitRunner{}
	var gotBranch string
	resolver := func(owner, repo string, number int) Config {
		cfg := defaultBranchConfig(owner, repo, number)
		cfg.Branch = "custom-branch"
		gotBranch = cfg.Branch
		return cfg
	}

	h := NewOrphanHandler("hub-orphan", st, runner, nil, WithBranchResolver(resolver))
	_ = h.TriggerOrphanRebase(context.Background(), "acme", "widget", 42, "abc123")
	if gotBranch != "custom-branch" {
		t.Errorf("expected custom-branch, got %q", gotBranch)
	}
}
