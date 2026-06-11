package rebase

import (
	"context"
	"fmt"
	"os/exec"
)

// ExecRunner implements GitRunner by shelling out to the system git binary.
// Use this in production binaries; inject a fake in unit tests.
type ExecRunner struct {
	// Dir is the working directory for all git commands.
	// If empty, the current working directory is used.
	Dir string
}

// NewExecRunner constructs an ExecRunner for the given repository directory.
func NewExecRunner(dir string) *ExecRunner {
	return &ExecRunner{Dir: dir}
}

// Fetch runs: git fetch <remote>
func (r *ExecRunner) Fetch(ctx context.Context, remote string) error {
	return r.run(ctx, "fetch", remote)
}

// Rebase runs: git rebase <upstream> <branch>
// The branch is checked out first if it differs from HEAD.
func (r *ExecRunner) Rebase(ctx context.Context, branch, upstream string) error {
	// Checkout the branch, then rebase onto upstream.
	if err := r.run(ctx, "checkout", branch); err != nil {
		return fmt.Errorf("checkout %s: %w", branch, err)
	}
	return r.run(ctx, "rebase", upstream)
}

// ForcePushWithLease runs: git push --force-with-lease <remote> <branch>
func (r *ExecRunner) ForcePushWithLease(ctx context.Context, remote, branch string) error {
	return r.run(ctx, "push", "--force-with-lease", remote, branch)
}

func (r *ExecRunner) run(ctx context.Context, args ...string) error {
	//nolint:gosec // args are constructed internally, not from user input
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", args[0], err, out)
	}
	return nil
}
