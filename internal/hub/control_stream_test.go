// Auth-v2 Phase 3.5 (issue #60): webhook ingest fan-out tests for the
// control stream. These tests stand up a Hub against an in-memory store with
// a fake ControlPublisher, drive pull_request / installation_repositories
// webhooks through ingest, and assert the publisher saw the right events
// (or didn't, for the cross-user-isolation and legacy-row cases).
package hub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ravencloak-org/caw/internal/github"
	"github.com/ravencloak-org/caw/internal/store"
)

// fakeControlPub records every Publish call for assertion.
type fakeControlPub struct {
	mu     sync.Mutex
	events []controlCall
}

type controlCall struct {
	UserID int64
	Name   string
	Data   map[string]any
}

func (f *fakeControlPub) Publish(userID int64, name string, data []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parsed map[string]any
	_ = json.Unmarshal(data, &parsed)
	f.events = append(f.events, controlCall{UserID: userID, Name: name, Data: parsed})
	return 1
}

func (f *fakeControlPub) calls() []controlCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]controlCall, len(f.events))
	copy(out, f.events)
	return out
}

// seedUserToken inserts an active user-bound token for userID on installID.
func seedUserToken(t *testing.T, s *store.Store, hash, installID string, userID int64) {
	t.Helper()
	if err := s.InsertTokenRow(store.Token{
		ID:             hash + "00000000000000000000000000",
		Hash:           hash,
		InstallationID: installID,
		GitHubUserID:   &userID,
		DeviceLabel:    "test-dev",
		CreatedAt:      1_700_000_000,
	}); err != nil {
		t.Fatalf("InsertTokenRow %s: %v", hash, err)
	}
}

// TestControlStream_PROpenedFansOut drives a pull_request.opened webhook and
// asserts the control publisher received one pr_opened event keyed on the
// sender's user id, with the expected wire-format payload.
func TestControlStream_PROpenedFansOut(t *testing.T) {
	st := openTestStore(t)
	cp := &fakeControlPub{}
	h := New(st, nil, nil).WithControlPublisher(cp)

	uid := int64(12345)
	seedUserToken(t, st, "userA", "inst-1", uid)

	env := github.Envelope{Action: "opened"}
	env.Repository.Name = "caw"
	env.Repository.Owner.Login = "ravencloak-org"
	env.PullRequest = &github.PullRequest{Number: 56}
	env.PullRequest.Head.SHA = "abc123"
	env.PullRequest.User.Login = "jobinlawrance"
	env.Sender.ID = uid
	env.Sender.Login = "jobinlawrance"

	if err := h.ingest(context.Background(), "pull_request", env); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	calls := cp.calls()
	if len(calls) != 1 {
		t.Fatalf("publish calls = %d, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.UserID != uid {
		t.Errorf("user id = %d, want %d", got.UserID, uid)
	}
	if got.Name != "pr_opened" {
		t.Errorf("event = %q, want pr_opened", got.Name)
	}
	if got.Data["owner"] != "ravencloak-org" ||
		got.Data["repo"] != "caw" ||
		int(got.Data["number"].(float64)) != 56 ||
		got.Data["head_sha"] != "abc123" ||
		got.Data["author_login"] != "jobinlawrance" {
		t.Errorf("payload = %+v, missing expected fields", got.Data)
	}
}

// TestControlStream_PRClosedFansOut drives pull_request.closed and asserts
// a pr_closed event is published with the minimal payload.
func TestControlStream_PRClosedFansOut(t *testing.T) {
	st := openTestStore(t)
	cp := &fakeControlPub{}
	h := New(st, nil, nil).WithControlPublisher(cp)

	uid := int64(7)
	seedUserToken(t, st, "userC", "inst-1", uid)

	env := github.Envelope{Action: "closed"}
	env.Repository.Name = "r"
	env.Repository.Owner.Login = "o"
	env.PullRequest = &github.PullRequest{Number: 9}
	env.PullRequest.Head.SHA = "sha"
	env.Sender.ID = uid

	if err := h.ingest(context.Background(), "pull_request", env); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	calls := cp.calls()
	if len(calls) != 1 || calls[0].Name != "pr_closed" {
		t.Fatalf("calls = %+v, want one pr_closed", calls)
	}
	if calls[0].UserID != uid {
		t.Errorf("user = %d, want %d", calls[0].UserID, uid)
	}
}

// TestControlStream_DoesNotLeakAcrossUsers — two users, two PRs, each
// receives only their own. Cross-user isolation invariant from the plan.
func TestControlStream_DoesNotLeakAcrossUsers(t *testing.T) {
	st := openTestStore(t)
	cp := &fakeControlPub{}
	h := New(st, nil, nil).WithControlPublisher(cp)

	alice, bob := int64(1), int64(2)
	seedUserToken(t, st, "alice", "inst-1", alice)
	seedUserToken(t, st, "bob", "inst-1", bob)

	mkPR := func(sender int64, num int) github.Envelope {
		env := github.Envelope{Action: "opened"}
		env.Repository.Name = "r"
		env.Repository.Owner.Login = "o"
		env.PullRequest = &github.PullRequest{Number: num}
		env.PullRequest.Head.SHA = "s"
		env.Sender.ID = sender
		return env
	}
	if err := h.ingest(context.Background(), "pull_request", mkPR(alice, 1)); err != nil {
		t.Fatalf("ingest alice: %v", err)
	}
	if err := h.ingest(context.Background(), "pull_request", mkPR(bob, 2)); err != nil {
		t.Fatalf("ingest bob: %v", err)
	}
	calls := cp.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2: %+v", len(calls), calls)
	}
	// Each call MUST address only the matching user.
	for _, c := range calls {
		num := int(c.Data["number"].(float64))
		switch num {
		case 1:
			if c.UserID != alice {
				t.Errorf("PR #1 fanned to user %d, want alice %d", c.UserID, alice)
			}
		case 2:
			if c.UserID != bob {
				t.Errorf("PR #2 fanned to user %d, want bob %d", c.UserID, bob)
			}
		}
	}
}

// TestControlStream_NoUserBoundTokenSkipsPublish — a webhook from a sender
// who has no user-bound token on file MUST NOT publish (avoids producing
// events that no MCP plugin can consume).
func TestControlStream_NoUserBoundTokenSkipsPublish(t *testing.T) {
	st := openTestStore(t)
	cp := &fakeControlPub{}
	h := New(st, nil, nil).WithControlPublisher(cp)

	env := github.Envelope{Action: "opened"}
	env.Repository.Name = "r"
	env.Repository.Owner.Login = "o"
	env.PullRequest = &github.PullRequest{Number: 1}
	env.PullRequest.Head.SHA = "s"
	env.Sender.ID = 999 // no token row for this user

	if err := h.ingest(context.Background(), "pull_request", env); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if calls := cp.calls(); len(calls) != 0 {
		t.Fatalf("calls = %d, want 0: %+v", len(calls), calls)
	}
}

// TestControlStream_InstallationAddedFansOut — installation_repositories.added
// publishes installation_added to every user with an active token on the
// installation, de-duped per github_user_id.
func TestControlStream_InstallationAddedFansOut(t *testing.T) {
	st := openTestStore(t)
	cp := &fakeControlPub{}
	h := New(st, nil, nil).WithControlPublisher(cp)

	// Two users with active tokens; user A has two tokens (two devices) to
	a, b := int64(1), int64(2)
	seedUserToken(t, st, "a1", "1", a)
	seedUserToken(t, st, "a2", "1", a)
	seedUserToken(t, st, "b1", "1", b)

	env := github.Envelope{Action: "added"}
	env.Installation = &github.Installation{ID: 1}
	env.Installation.Account.Login = "ravencloak-org"
	env.RepositoriesAdded = []github.Repository{{FullName: "ravencloak-org/new-repo"}}

	if err := h.handleInstallationRepositories(env); err != nil {
		t.Fatalf("handleInstallationRepositories: %v", err)
	}
	calls := cp.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one per distinct user): %+v", len(calls), calls)
	}
	seen := map[int64]bool{}
	for _, c := range calls {
		seen[c.UserID] = true
		if c.Name != "installation_added" {
			t.Errorf("event = %q, want installation_added", c.Name)
		}
		if c.Data["installation_id"] != "1" || c.Data["org"] != "ravencloak-org" {
			t.Errorf("payload = %+v", c.Data)
		}
	}
	if !seen[a] || !seen[b] {
		t.Errorf("missing fan-out to one of [a=%d b=%d]; seen=%v", a, b, seen)
	}
}
