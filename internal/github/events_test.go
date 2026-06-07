package github

import (
	"encoding/json"
	"testing"
)

func TestUnmarshalPullRequestEnvelope(t *testing.T) {
	data := []byte(`{
		"action": "synchronize",
		"number": 42,
		"repository": {"name": "caw", "owner": {"login": "ravencloak-org"}},
		"pull_request": {"number": 42, "state": "open", "head": {"sha": "deadbeef"}}
	}`)

	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.Action != "synchronize" || e.Repository.Owner.Login != "ravencloak-org" || e.Repository.Name != "caw" {
		t.Fatalf("envelope basics wrong: %+v", e)
	}
	if e.PullRequest == nil {
		t.Fatal("PullRequest should be non-nil")
	}
	if e.PullRequest.Number != 42 || e.PullRequest.Head.SHA != "deadbeef" || e.PullRequest.State != "open" {
		t.Fatalf("pull_request fields wrong: %+v", *e.PullRequest)
	}
	if e.CheckSuite != nil {
		t.Fatal("CheckSuite should be nil for a pull_request payload")
	}
}

func TestUnmarshalCheckSuiteEnvelope(t *testing.T) {
	data := []byte(`{
		"action": "completed",
		"repository": {"name": "caw", "owner": {"login": "ravencloak-org"}},
		"check_suite": {"head_sha": "cafef00d", "pull_requests": [{"number": 7}]}
	}`)

	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.CheckSuite == nil {
		t.Fatal("CheckSuite should be non-nil")
	}
	if e.CheckSuite.HeadSHA != "cafef00d" || len(e.CheckSuite.PullRequests) != 1 || e.CheckSuite.PullRequests[0].Number != 7 {
		t.Fatalf("check_suite fields wrong: %+v", *e.CheckSuite)
	}
	if e.PullRequest != nil {
		t.Fatal("PullRequest should be nil for a check_suite payload")
	}
}

func TestUnmarshalNonPREnvelope(t *testing.T) {
	// e.g. a ping event: no pull_request / check_suite.
	data := []byte(`{"zen": "Keep it simple", "repository": {"name": "caw", "owner": {"login": "o"}}}`)
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if e.PullRequest != nil || e.CheckSuite != nil {
		t.Fatal("ping envelope should have neither pull_request nor check_suite")
	}
}
