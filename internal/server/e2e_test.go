package server_test

// End-to-end tests that wire the full Hub server (webhook ingest → Round
// settle → mergeability poll → compile → SSE fan-out / pending store / orphan
// rebase / lease lifecycle) and stub only the true external boundaries:
//   - the GitHub REST API (mergeability poll) via an httptest server, and
//   - the git CLI + GitHub auto-merge PATCH via fakes behind the rebase
//     interfaces.
//
// These complement TestEndToEnd_WebhookToSSE / TestEndToEnd_OrphanToPending in
// server_test.go (which exercise the no-poller live + pending paths) by proving
// the remaining slices integrate: mergeability (Slice 3), severity-bearing
// compile (Slice 7 ladder), orphan rebase fallback under the Hub-granted lease
// (Slice 6, ADR-0002/0005/0007), and the lease HTTP surface (ADR-0005).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/ghclient"
	"github.com/ravencloak-org/caw/internal/mergeability"
	"github.com/ravencloak-org/caw/internal/rebase"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/store"
)

// fakeGitHub stands up a GitHub REST stub that answers the one outbound call
// the Hub makes — GET /repos/{owner}/{repo}/pulls/{n} for the mergeability
// poll — with the given mergeable_state.
func fakeGitHub(t *testing.T, mergeableState string) *httptest.Server {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"state":"open","mergeable":true,"mergeable_state":%q}`, mergeableState)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(gh.Close)
	return gh
}

// fakeRunner records the git operations the orphan handler drives without
// shelling out to real git.
type fakeRunner struct {
	mu                       sync.Mutex
	fetch, rebase, forcePush int
}

func (f *fakeRunner) Fetch(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetch++
	return nil
}

func (f *fakeRunner) Rebase(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebase++
	return nil
}

func (f *fakeRunner) ForcePushWithLease(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forcePush++
	return nil
}

func (f *fakeRunner) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetch, f.rebase, f.forcePush
}

// fakeMerger records EnableAutoMerge invocations (the GitHub auto-merge PATCH).
type fakeMerger struct {
	mu     sync.Mutex
	calls  int
	number int
}

func (m *fakeMerger) EnableAutoMerge(_ context.Context, _, _ string, number int, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.number = number
	return nil
}

func (m *fakeMerger) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// e2eSummary mirrors compile.Summary for decoding the SSE payload.
type e2eSummary struct {
	Key    string `json:"key"`
	Seq    int    `json:"seq"`
	Groups []struct {
		Type    string   `json:"type"`
		Sources []string `json:"sources"`
		Count   int      `json:"count"`
		Items   []struct {
			Source   string `json:"source"`
			Severity string `json:"severity"`
			Body     string `json:"body"`
		} `json:"items"`
	} `json:"groups"`
	Text string `json:"text"`
}

// readSummary scans an SSE body for the first data: event and decodes it.
func readSummary(t *testing.T, body io.Reader) e2eSummary {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var s e2eSummary
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &s); err != nil {
			t.Fatalf("decode summary: %v", err)
		}
		return s
	}
	t.Fatal("no SSE data event received before timeout")
	return e2eSummary{}
}

// TestE2E_LivePath_ChecksAndMergeability: a subscribed Session receives a
// compiled summary that folds the check_suite failure (Checks) AND the
// settle-time mergeability poll (Mergeability, "behind base") into one push.
func TestE2E_LivePath_ChecksAndMergeability(t *testing.T) {
	gh := fakeGitHub(t, "behind")
	ts, token, _ := newTestServerOpts(t, 30*time.Millisecond, func(_ *store.Store) []settle.Option {
		return []settle.Option{settle.WithPoller(mergeability.New(ghclient.New(gh.URL, "tok")))}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse/o/r/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, want 200", resp.StatusCode)
	}

	postWebhook(t, ts.URL, "check_suite", "d1", checkSuiteFailure)

	s := readSummary(t, resp.Body)
	if s.Key != "o/r#1@sha1" {
		t.Fatalf("summary key = %q, want o/r#1@sha1", s.Key)
	}

	var checks, merge bool
	for _, g := range s.Groups {
		switch g.Type {
		case "checks":
			if len(g.Sources) == 0 || g.Sources[0] != "CI" {
				t.Fatalf("checks sources = %v, want [CI]", g.Sources)
			}
			checks = true
		case "mergeability":
			if len(g.Items) == 0 || g.Items[0].Body != "behind base" || g.Items[0].Source != "poll" {
				t.Fatalf("mergeability items = %+v, want one poll/behind base", g.Items)
			}
			merge = true
		}
	}
	if !checks || !merge {
		t.Fatalf("summary groups = %+v, want both checks and mergeability", s.Groups)
	}
}

// TestE2E_OrphanRebase_FullFallback: with no listener and a PR behind its base,
// the settle stores pending items AND drives the Hub orphan rebase under a
// store-granted lease (fetch/rebase/force-push), enables auto-merge, and
// releases the lease.
func TestE2E_OrphanRebase_FullFallback(t *testing.T) {
	gh := fakeGitHub(t, "behind")
	runner := &fakeRunner{}
	merger := &fakeMerger{}

	ts, token, st := newTestServerOpts(t, 15*time.Millisecond, func(st *store.Store) []settle.Option {
		poller := mergeability.New(ghclient.New(gh.URL, "tok"))
		oh := rebase.NewOrphanHandler("hub-orphan", st, runner, merger)
		return []settle.Option{settle.WithPoller(poller), settle.WithOrphanRebaseHandler(oh)}
	})

	postWebhook(t, ts.URL, "check_suite", "d1", checkSuiteFailure)

	// Wait for the orphan rebase to run end-to-end.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, rb, fp := runner.counts()
		if f > 0 && rb > 0 && fp > 0 && merger.count() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f, rb, fp := runner.counts(); f == 0 || rb == 0 || fp == 0 {
		t.Fatalf("git ops = fetch:%d rebase:%d push:%d, want all > 0", f, rb, fp)
	}
	if merger.count() == 0 {
		t.Fatal("auto-merge never enabled after rebase")
	}
	if merger.number != 1 {
		t.Fatalf("auto-merge PR number = %d, want 1", merger.number)
	}

	// Lease is released after the orphan rebase completes.
	leaseGone := false
	ld := time.Now().Add(2 * time.Second)
	for time.Now().Before(ld) {
		if _, ok, err := st.GetLease("o", "r", 1); err == nil && !ok {
			leaseGone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !leaseGone {
		t.Fatal("orphan lease was not released after rebase")
	}

	// Pending items persisted for the orphaned PR (latest per signal-type).
	items := getPending(t, ts.URL, token)
	types := map[string]bool{}
	for _, it := range items {
		types[it.SignalType] = true
	}
	if !types["checks"] || !types["mergeability"] {
		t.Fatalf("pending signal-types = %v, want both checks and mergeability", types)
	}
}

// TestE2E_LeaseLifecycle_OverHTTP: the rebase-lease HTTP surface enforces
// single ownership across installations — acquire, deny-other, heartbeat,
// release, re-acquire (ADR-0005).
func TestE2E_LeaseLifecycle_OverHTTP(t *testing.T) {
	ts, token1, st := newTestServerOpts(t, time.Second, nil)

	// Register a second installation scoped to the same repo.
	raw2, hash2, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("token2: %v", err)
	}
	if err := st.InsertToken(hash2, "inst2", "org2"); err != nil {
		t.Fatalf("insert token2: %v", err)
	}
	if err := st.UpsertInstallation("inst2", "org2"); err != nil {
		t.Fatalf("upsert installation2: %v", err)
	}
	if err := st.AddInstallationRepo("inst2", "o/r"); err != nil {
		t.Fatalf("add installation repo2: %v", err)
	}

	base := ts.URL + "/leases/o/r/1"

	// inst1 acquires.
	if code, body := leaseReq(t, http.MethodPost, base, token1); code != http.StatusOK {
		t.Fatalf("inst1 acquire = %d, want 200 (body %v)", code, body)
	} else if body["granted"] != true || body["holder"] != "inst1" {
		t.Fatalf("inst1 acquire body = %v, want granted/inst1", body)
	}

	// inst2 is denied while inst1 holds it.
	if code, body := leaseReq(t, http.MethodPost, base, raw2); code != http.StatusConflict {
		t.Fatalf("inst2 acquire = %d, want 409 (body %v)", code, body)
	} else if body["granted"] != false || body["holder"] != "inst1" {
		t.Fatalf("inst2 denial body = %v, want not-granted/holder inst1", body)
	}

	// inst1 heartbeats successfully.
	if code, body := leaseReq(t, http.MethodPut, base+"/heartbeat", token1); code != http.StatusOK {
		t.Fatalf("inst1 heartbeat = %d, want 200 (body %v)", code, body)
	}

	// inst1 releases.
	if code, _ := leaseReq(t, http.MethodDelete, base, token1); code != http.StatusNoContent {
		t.Fatalf("inst1 release = %d, want 204", code)
	}

	// inst2 can now acquire.
	if code, body := leaseReq(t, http.MethodPost, base, raw2); code != http.StatusOK {
		t.Fatalf("inst2 re-acquire = %d, want 200 (body %v)", code, body)
	} else if body["granted"] != true || body["holder"] != "inst2" {
		t.Fatalf("inst2 re-acquire body = %v, want granted/inst2", body)
	}
}

// leaseReq performs a lease HTTP request and decodes the JSON body (empty for
// 204 No Content).
func leaseReq(t *testing.T, method, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lease %s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if resp.StatusCode != http.StatusNoContent {
		_ = json.NewDecoder(resp.Body).Decode(&body)
	}
	return resp.StatusCode, body
}
