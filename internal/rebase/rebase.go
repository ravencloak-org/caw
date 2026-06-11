// Package rebase provides the GitRunner interface and shared types for the
// session-side rebase (normal path, ADR-0002) and the Hub orphan fallback.
// All git operations are behind the GitRunner interface so that unit tests
// never shell out to real git or hit the network.
package rebase

import "context"

// GitRunner abstracts the git operations required to rebase a PR branch onto
// the current base-branch HEAD. Implementations may be real (exec.Command) or
// test fakes.
type GitRunner interface {
	// Fetch updates the local remote-tracking refs for the given remote.
	Fetch(ctx context.Context, remote string) error

	// Rebase rebases the given branch onto the upstream tracking ref.
	// branch is the local branch name (e.g. "feature/my-work"),
	// upstream is the tracking ref (e.g. "origin/main").
	Rebase(ctx context.Context, branch, upstream string) error

	// ForcePushWithLease pushes branch to remote using --force-with-lease
	// so that the push is rejected if the remote has moved unexpectedly.
	ForcePushWithLease(ctx context.Context, remote, branch string) error
}

// Config holds the parameters for a single rebase operation.
type Config struct {
	Owner  string
	Repo   string
	Number int
	SHA    string // the commit SHA that triggered the rebase
	Branch string // local branch to rebase
	Remote string // git remote name (usually "origin")
	Base   string // upstream tracking ref (e.g. "origin/main")
	Holder string // lease holder identifier
}
