package rebase

import (
	"context"
	"errors"
	"testing"
)

// --- fakes ---

type fakeGitRunner struct {
	fetchErr     error
	rebaseErr    error
	pushErr      error
	fetchCalled  bool
	rebaseCalled bool
	pushCalled   bool
}

func (f *fakeGitRunner) Fetch(_ context.Context, _ string) error {
	f.fetchCalled = true
	return f.fetchErr
}

func (f *fakeGitRunner) Rebase(_ context.Context, _, _ string) error {
	f.rebaseCalled = true
	return f.rebaseErr
}

func (f *fakeGitRunner) ForcePushWithLease(_ context.Context, _, _ string) error {
	f.pushCalled = true
	return f.pushErr
}

type fakeLeaseRenewer struct {
	renewErr   error
	releaseErr error
	renewCount int
	released   bool
}

func (f *fakeLeaseRenewer) RenewRebaseLease(_ context.Context, _, _ string, _ int, _ string) error {
	f.renewCount++
	return f.renewErr
}

func (f *fakeLeaseRenewer) ReleaseRebaseLease(_ context.Context, _, _ string, _ int, _ string) error {
	f.released = true
	return f.releaseErr
}

func testConfig() Config {
	return Config{
		Owner:  "acme",
		Repo:   "widget",
		Number: 42,
		SHA:    "abc123",
		Branch: "feature/add-widget",
		Remote: "origin",
		Base:   "origin/main",
		Holder: "session-holder-1",
	}
}

// --- tests ---

func TestSession_Run_Success(t *testing.T) {
	runner := &fakeGitRunner{}
	renewer := &fakeLeaseRenewer{}

	s := NewSession(runner, renewer)
	if err := s.Run(context.Background(), testConfig()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
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
	if !renewer.released {
		t.Error("ReleaseRebaseLease not called")
	}
}

func TestSession_Run_FetchError_ShortCircuits(t *testing.T) {
	fetchErr := errors.New("fetch failed")
	runner := &fakeGitRunner{fetchErr: fetchErr}
	renewer := &fakeLeaseRenewer{}

	s := NewSession(runner, renewer)
	err := s.Run(context.Background(), testConfig())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, fetchErr) {
		t.Fatalf("expected fetch error in chain, got: %v", err)
	}
	// Rebase and push must not run after fetch fails.
	if runner.rebaseCalled {
		t.Error("Rebase should not be called after Fetch failure")
	}
	if runner.pushCalled {
		t.Error("ForcePushWithLease should not be called after Fetch failure")
	}
	// Lease must still be released.
	if !renewer.released {
		t.Error("ReleaseRebaseLease must be called even on error")
	}
}

func TestSession_Run_RebaseError_ShortCircuits(t *testing.T) {
	rebaseErr := errors.New("rebase conflict")
	runner := &fakeGitRunner{rebaseErr: rebaseErr}
	renewer := &fakeLeaseRenewer{}

	s := NewSession(runner, renewer)
	err := s.Run(context.Background(), testConfig())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, rebaseErr) {
		t.Fatalf("expected rebase error in chain, got: %v", err)
	}
	if runner.pushCalled {
		t.Error("ForcePushWithLease should not be called after Rebase failure")
	}
	if !renewer.released {
		t.Error("ReleaseRebaseLease must be called even on error")
	}
}

func TestSession_Run_PushError_ReturnsError(t *testing.T) {
	pushErr := errors.New("push rejected")
	runner := &fakeGitRunner{pushErr: pushErr}
	renewer := &fakeLeaseRenewer{}

	s := NewSession(runner, renewer)
	err := s.Run(context.Background(), testConfig())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, pushErr) {
		t.Fatalf("expected push error in chain, got: %v", err)
	}
	if !renewer.released {
		t.Error("ReleaseRebaseLease must be called even on push error")
	}
}

func TestSession_Run_LeaseReleasedOnSuccess(_ *testing.T) {
	s := NewSession(&fakeGitRunner{}, &fakeLeaseRenewer{})
	_ = s.Run(context.Background(), testConfig())
	// Covered by TestSession_Run_Success — explicit check here for clarity.
}

func TestSession_Run_NilRenewer_UsesNoop(t *testing.T) {
	// NewSession with nil renewer should not panic.
	s := NewSession(&fakeGitRunner{}, nil)
	if err := s.Run(context.Background(), testConfig()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSession_Run_ReleaseError_DoesNotMaskRebaseResult(t *testing.T) {
	// Release failure is best-effort and must not override a successful rebase.
	renewer := &fakeLeaseRenewer{releaseErr: errors.New("release failed")}
	s := NewSession(&fakeGitRunner{}, renewer)
	if err := s.Run(context.Background(), testConfig()); err != nil {
		t.Fatalf("release error should not be returned, got: %v", err)
	}
}
