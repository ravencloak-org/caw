package server_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/repoaccess"
	"github.com/ravencloak-org/caw/internal/server"
	"github.com/ravencloak-org/caw/internal/settle"
	"github.com/ravencloak-org/caw/internal/sse"
	"github.com/ravencloak-org/caw/internal/store"
)

func init() { gin.SetMode(gin.TestMode) }

const secret = "itest"

// checkSuiteFailure is a check_suite=completed/failure payload for o/r#1@sha1.
var checkSuiteFailure = []byte(`{
	"action":"completed",
	"check_suite":{"id":99,"head_sha":"sha1","status":"completed","conclusion":"failure","app":{"name":"CI"},"pull_requests":[{"number":1}]},
	"repository":{"name":"r","owner":{"login":"o"}}
}`)

// newTestServerOpts builds the full Hub server on a temp SQLite store with one
// installation (inst1/org1) scoped to repo o/r and a valid token. optsFn, when
// non-nil, receives the store and returns the settle options to wire (poller,
// orphan handler, …) so e2e tests can inject GitHub-API-backed behavior.
func newTestServerOpts(t *testing.T, grace time.Duration, optsFn func(*store.Store) []settle.Option) (*httptest.Server, string, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	raw, hash, err := auth.GenerateToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := st.InsertTokenRow(store.Token{Hash: hash, InstallationID: "inst1", Org: "org1"}); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	// Register inst1's installation and associate the repo used by the
	// integration tests (owner=o, repo=r) so RequireRepoScope and
	// ListPendingForInstallation find the repo in installation_repos.
	if err := st.UpsertInstallation("inst1", "org1"); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	if err := st.AddInstallationRepo("inst1", "o/r"); err != nil {
		t.Fatalf("add installation repo: %v", err)
	}

	sseHub := sse.New()
	var opts []settle.Option
	if optsFn != nil {
		opts = optsFn(st)
	}
	engine := settle.New(st, sseHub, grace, opts...)
	// Existing tests use legacy (NULL github_user_id) tokens — they bypass
	// the Auth v2 RequireRepoAccess middleware before ever touching the
	// cache's Checker seam, so a checker-less cache is sufficient here.
	cache := repoaccess.NewCache(nil, repoaccess.Options{})
	ts := httptest.NewServer(server.New(st, sseHub, engine, []byte(secret), nil, nil, nil, cache))
	t.Cleanup(ts.Close)
	return ts, raw, st
}

func newTestServer(t *testing.T, grace time.Duration) (*httptest.Server, string) {
	ts, raw, _ := newTestServerOpts(t, grace, nil)
	return ts, raw
}

func sign(payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, url, event, delivery string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status = %d, want 202", resp.StatusCode)
	}
}

// TestEndToEnd_WebhookToSSE: a subscribed Session receives the compiled summary
// pushed after the Round settles.
func TestEndToEnd_WebhookToSSE(t *testing.T) {
	ts, token := newTestServer(t, 30*time.Millisecond)

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
	// Subscription is registered (headers flushed). Now trigger a settle.
	postWebhook(t, ts.URL, "check_suite", "d1", checkSuiteFailure)

	data := ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") {
			data = strings.TrimPrefix(line, "data:")
			break
		}
	}
	if data == "" {
		t.Fatal("no SSE data event received before timeout")
	}
	var summary struct {
		Key    string `json:"key"`
		Groups []struct {
			Type    string   `json:"type"`
			Sources []string `json:"sources"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(data), &summary); err != nil {
		t.Fatalf("decode summary %q: %v", data, err)
	}
	if summary.Key != "o/r#1@sha1" {
		t.Fatalf("summary key = %q, want o/r#1@sha1", summary.Key)
	}
	if len(summary.Groups) != 1 || summary.Groups[0].Type != "checks" || summary.Groups[0].Sources[0] != "CI" {
		t.Fatalf("summary groups = %+v, want one checks/CI group", summary.Groups)
	}
}

// TestEndToEnd_OrphanToPending: with no listener, the summary lands in pending
// and get_pending returns it.
func TestEndToEnd_OrphanToPending(t *testing.T) {
	ts, token := newTestServer(t, 15*time.Millisecond)

	postWebhook(t, ts.URL, "check_suite", "d1", checkSuiteFailure)

	deadline := time.Now().Add(5 * time.Second)
	var items []store.PendingItem
	for time.Now().Before(deadline) {
		items = getPending(t, ts.URL, token)
		if len(items) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(items) != 1 {
		t.Fatalf("pending items = %d, want 1", len(items))
	}
	if items[0].SignalType != "checks" || items[0].SHA != "sha1" {
		t.Fatalf("pending item = %+v", items[0])
	}
}

// TestPendingAuth: get_pending requires a valid installation token.
func TestPendingAuth(t *testing.T) {
	ts, token := newTestServer(t, time.Second)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"bad token", "Bearer not-a-real-token", http.StatusUnauthorized},
		{"valid token", "Bearer " + token, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+"/pending", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func getPending(t *testing.T, url, token string) []store.PendingItem {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/pending", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pending status = %d", resp.StatusCode)
	}
	var body struct {
		Items []store.PendingItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	return body.Items
}

// TestLandingPage: GET / returns the embedded static landing page, with no
// auth token in the request — proving the route is registered before the
// auth middleware.
func TestLandingPage(t *testing.T) {
	ts, _ := newTestServer(t, time.Hour)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html prefix", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "caw") {
		t.Fatalf("body missing %q", "caw")
	}
	if !strings.Contains(s, "github.com/ravencloak-org/caw") {
		t.Fatalf("body missing %q", "github.com/ravencloak-org/caw")
	}
}
