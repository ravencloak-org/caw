package watcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/watcher"
)

// --- helpers ---

// fakeHub builds a test HTTP server that stands in for the Hub.
type fakeHub struct {
	*httptest.Server
	// pendingResp is the JSON body returned by GET /pending.
	pendingResp string
	// leaseStatusCode is the HTTP status returned by POST /leases/:owner/:repo/:number.
	leaseStatusCode int
	// leaseBody is the JSON body returned by the lease endpoint.
	leaseBody string
	// renewStatusCode is the HTTP status returned by PUT .../heartbeat.
	renewStatusCode int
	// renewBody is the JSON body returned by the renew endpoint.
	renewBody string
	// releaseStatusCode is the HTTP status returned by DELETE /leases/...
	releaseStatusCode int
	// sseMessages are sent to SSE subscribers in order then the stream is closed.
	sseMessages []string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	fh := &fakeHub{
		pendingResp:       `{"items":[]}`,
		leaseStatusCode:   http.StatusOK,
		leaseBody:         `{"granted":true,"holder":"inst-A","expires_at":9999999999,"last_heartbeat_at":0,"acquired_at":1}`,
		renewStatusCode:   http.StatusOK,
		renewBody:         `{"granted":true,"holder":"inst-A","expires_at":9999999999,"last_heartbeat_at":1,"acquired_at":1}`,
		releaseStatusCode: http.StatusNoContent,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/pending", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Verify Bearer token is present.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, fh.pendingResp)
	})
	mux.HandleFunc("/leases/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(fh.leaseStatusCode)
			_, _ = fmt.Fprint(w, fh.leaseBody)
		case http.MethodPut:
			// heartbeat endpoint: PUT /leases/:owner/:repo/:number/heartbeat
			w.WriteHeader(fh.renewStatusCode)
			_, _ = fmt.Fprint(w, fh.renewBody)
		case http.MethodDelete:
			w.WriteHeader(fh.releaseStatusCode)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/sse/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		for _, msg := range fh.sseMessages {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	fh.Server = httptest.NewServer(mux)
	t.Cleanup(fh.Close)
	return fh
}

func newTestClient(t *testing.T, h *fakeHub) *watcher.Client {
	t.Helper()
	return watcher.NewClient(h.URL, "test-token-abc")
}

// --- GetPending tests ---

func TestGetPending_EmptyList(t *testing.T) {
	fh := newFakeHub(t)
	c := newTestClient(t, fh)

	items, err := c.GetPending(context.Background())
	if err != nil {
		t.Fatalf("GetPending: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty items, got %d", len(items))
	}
}

func TestGetPending_ReturnsItems(t *testing.T) {
	fh := newFakeHub(t)
	fh.pendingResp = `{"items":[
		{"owner":"org","repo":"r","number":1,"signal_type":"review","sha":"abc","pr_state":"open","summary":"s","updated_at":1},
		{"owner":"org","repo":"r","number":2,"signal_type":"ci","sha":"def","pr_state":"open","summary":"t","updated_at":2}
	]}`
	c := newTestClient(t, fh)

	items, err := c.GetPending(context.Background())
	if err != nil {
		t.Fatalf("GetPending: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Owner != "org" || items[0].Repo != "r" {
		t.Errorf("item[0] = %+v, want org/r", items[0])
	}
}

func TestGetPending_SendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[]}`)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "my-secret-token")
	_, err := c.GetPending(context.Background())
	if err != nil {
		t.Fatalf("GetPending: %v", err)
	}
	if gotAuth != "Bearer my-secret-token" {
		t.Errorf("Authorization = %q, want 'Bearer my-secret-token'", gotAuth)
	}
}

func TestGetPending_HTTPError_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "tok")
	_, err := c.GetPending(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// --- AcquireRebaseLease tests ---

func TestAcquireRebaseLease_Granted(t *testing.T) {
	fh := newFakeHub(t)
	fh.leaseStatusCode = http.StatusOK
	fh.leaseBody = `{"granted":true,"holder":"inst-A","expires_at":9999,"last_heartbeat_at":0,"acquired_at":1}`
	c := newTestClient(t, fh)

	res, err := c.AcquireRebaseLease(context.Background(), "org", "repo", 1)
	if err != nil {
		t.Fatalf("AcquireRebaseLease: %v", err)
	}
	if !res.Granted {
		t.Fatalf("expected granted=true")
	}
	if res.Holder != "inst-A" {
		t.Errorf("holder = %q, want inst-A", res.Holder)
	}
}

func TestAcquireRebaseLease_Denied(t *testing.T) {
	fh := newFakeHub(t)
	fh.leaseStatusCode = http.StatusConflict
	fh.leaseBody = `{"granted":false,"holder":"inst-B","expires_at":9999,"last_heartbeat_at":0,"acquired_at":1}`
	c := newTestClient(t, fh)

	res, err := c.AcquireRebaseLease(context.Background(), "org", "repo", 2)
	if err != nil {
		t.Fatalf("AcquireRebaseLease: %v", err)
	}
	if res.Granted {
		t.Fatal("expected granted=false for 409")
	}
	if res.Holder != "inst-B" {
		t.Errorf("holder = %q, want inst-B", res.Holder)
	}
}

func TestAcquireRebaseLease_SendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"granted":true,"holder":"x","expires_at":1,"last_heartbeat_at":0,"acquired_at":1}`)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "my-tok")
	_, err := c.AcquireRebaseLease(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("AcquireRebaseLease: %v", err)
	}
	if gotAuth != "Bearer my-tok" {
		t.Errorf("Authorization = %q, want 'Bearer my-tok'", gotAuth)
	}
}

func TestAcquireRebaseLease_ServerError_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "tok")
	_, err := c.AcquireRebaseLease(context.Background(), "o", "r", 1)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// --- SubscribePR tests ---

func TestSubscribePR_ReceivesMessages(t *testing.T) {
	// Build a fake summary with one signal group.
	summary := map[string]interface{}{
		"key": "org/repo#1@abc",
		"seq": 1,
		"groups": []interface{}{
			map[string]interface{}{
				"type":    "review",
				"sources": []string{"bot"},
				"count":   1,
				"items": []interface{}{
					map[string]interface{}{"source": "bot", "severity": "minor", "body": "nit"},
				},
			},
		},
		"text": "org/repo#1@abc seq=1\n- review: 1 (bot)",
	}
	rawMsg, _ := json.Marshal(summary)

	fh := newFakeHub(t)
	fh.sseMessages = []string{string(rawMsg)}
	c := newTestClient(t, fh)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var received []watcher.SummaryMessage
	err := c.SubscribePR(ctx, "org", "repo", 1, func(msg watcher.SummaryMessage) {
		received = append(received, msg)
	})
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("SubscribePR: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("received %d messages, want 1", len(received))
	}
	if received[0].Key != "org/repo#1@abc" {
		t.Errorf("key = %q, want org/repo#1@abc", received[0].Key)
	}
	if received[0].Rendered == "" {
		t.Errorf("rendered text should not be empty")
	}
}

func TestSubscribePR_UsesCorrectSSEURL(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		// close immediately.
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = c.SubscribePR(ctx, "myorg", "myrepo", 42, func(_ watcher.SummaryMessage) {})

	if gotPath != "/sse/myorg/myrepo/42" {
		t.Errorf("path = %q, want /sse/myorg/myrepo/42", gotPath)
	}
}

func TestSubscribePR_SendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "watch-token")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = c.SubscribePR(ctx, "o", "r", 1, func(_ watcher.SummaryMessage) {})

	if gotAuth != "Bearer watch-token" {
		t.Errorf("Authorization = %q, want 'Bearer watch-token'", gotAuth)
	}
}

// --- RenewRebaseLease tests ---

func TestRenewRebaseLease_Success(t *testing.T) {
	fh := newFakeHub(t)
	fh.renewStatusCode = http.StatusOK
	fh.renewBody = `{"granted":true,"holder":"inst-A","expires_at":9999,"last_heartbeat_at":500,"acquired_at":1}`
	c := newTestClient(t, fh)

	res, err := c.RenewRebaseLease(context.Background(), "org", "repo", 1, "inst-A")
	if err != nil {
		t.Fatalf("RenewRebaseLease: %v", err)
	}
	if !res.Granted {
		t.Fatal("expected granted=true")
	}
	if res.Holder != "inst-A" {
		t.Errorf("holder = %q, want inst-A", res.Holder)
	}
	if res.LastHeartbeatAt != 500 {
		t.Errorf("last_heartbeat_at = %d, want 500", res.LastHeartbeatAt)
	}
}

func TestRenewRebaseLease_WrongHolder_ReturnsError(t *testing.T) {
	fh := newFakeHub(t)
	fh.renewStatusCode = http.StatusForbidden
	fh.renewBody = `{"error":"forbidden"}`
	c := newTestClient(t, fh)

	_, err := c.RenewRebaseLease(context.Background(), "org", "repo", 1, "inst-X")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestRenewRebaseLease_SendsBearerTokenAndHolder(t *testing.T) {
	var gotAuth, gotHolder string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHolder = r.Header.Get("X-Lease-Holder")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"granted":true,"holder":"inst-A","expires_at":9999,"last_heartbeat_at":1,"acquired_at":1}`)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "my-tok")
	_, err := c.RenewRebaseLease(context.Background(), "o", "r", 1, "inst-A")
	if err != nil {
		t.Fatalf("RenewRebaseLease: %v", err)
	}
	if gotAuth != "Bearer my-tok" {
		t.Errorf("Authorization = %q, want 'Bearer my-tok'", gotAuth)
	}
	if gotHolder != "inst-A" {
		t.Errorf("X-Lease-Holder = %q, want inst-A", gotHolder)
	}
}

func TestRenewRebaseLease_ServerError_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "tok")
	_, err := c.RenewRebaseLease(context.Background(), "o", "r", 1, "h")
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

// --- ReleaseRebaseLease tests ---

func TestReleaseRebaseLease_Success(t *testing.T) {
	fh := newFakeHub(t)
	fh.releaseStatusCode = http.StatusNoContent
	c := newTestClient(t, fh)

	if err := c.ReleaseRebaseLease(context.Background(), "org", "repo", 1, "inst-A"); err != nil {
		t.Fatalf("ReleaseRebaseLease: %v", err)
	}
}

func TestReleaseRebaseLease_WrongHolder_ReturnsError(t *testing.T) {
	fh := newFakeHub(t)
	fh.releaseStatusCode = http.StatusForbidden
	c := newTestClient(t, fh)

	err := c.ReleaseRebaseLease(context.Background(), "org", "repo", 1, "inst-X")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestReleaseRebaseLease_SendsBearerTokenAndHolder(t *testing.T) {
	var gotAuth, gotHolder, gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHolder = r.Header.Get("X-Lease-Holder")
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "my-tok")
	if err := c.ReleaseRebaseLease(context.Background(), "o", "r", 1, "inst-A"); err != nil {
		t.Fatalf("ReleaseRebaseLease: %v", err)
	}
	if gotAuth != "Bearer my-tok" {
		t.Errorf("Authorization = %q, want 'Bearer my-tok'", gotAuth)
	}
	if gotHolder != "inst-A" {
		t.Errorf("X-Lease-Holder = %q, want inst-A", gotHolder)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
}

func TestReleaseRebaseLease_ServerError_ReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	c := watcher.NewClient(ts.URL, "tok")
	if err := c.ReleaseRebaseLease(context.Background(), "o", "r", 1, "h"); err == nil {
		t.Fatal("expected error for 500")
	}
}
