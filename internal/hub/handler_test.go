package hub

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/store"
)

func init() { gin.SetMode(gin.TestMode) }

func newHarness(t *testing.T, secret []byte) (*gin.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := gin.New()
	r.POST("/webhooks/github", New(st, secret).HandleWebhook)
	return r, st
}

func post(r *gin.Engine, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var prBody = []byte(`{"action":"opened","repository":{"name":"caw","owner":{"login":"ravencloak-org"}},"pull_request":{"number":42,"state":"open","head":{"sha":"deadbeef"}}}`)

func TestHandleWebhook_ValidBucketsRound(t *testing.T) {
	secret := []byte("s3cr3t")
	r, st := newHarness(t, secret)

	w := post(r, prBody, map[string]string{
		"X-Hub-Signature-256": sign(secret, prBody),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "del-1",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%q", w.Code, w.Body.String())
	}
	exists, err := st.RoundExists("ravencloak-org", "caw", 42, "deadbeef")
	if err != nil || !exists {
		t.Fatalf("round not recorded (exists=%v err=%v)", exists, err)
	}
}

func TestHandleWebhook_DuplicateDelivery(t *testing.T) {
	secret := []byte("s3cr3t")
	r, _ := newHarness(t, secret)
	hdr := map[string]string{
		"X-Hub-Signature-256": sign(secret, prBody),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "dup",
	}
	if w := post(r, prBody, hdr); w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", w.Code)
	}
	w := post(r, prBody, hdr)
	if w.Code != http.StatusOK || w.Body.String() != "duplicate" {
		t.Fatalf("second status = %d body=%q, want 200 duplicate", w.Code, w.Body.String())
	}
}

func TestHandleWebhook_BadSignature(t *testing.T) {
	r, st := newHarness(t, []byte("s3cr3t"))
	w := post(r, prBody, map[string]string{
		"X-Hub-Signature-256": sign([]byte("wrong"), prBody),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "del-x",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if exists, _ := st.RoundExists("ravencloak-org", "caw", 42, "deadbeef"); exists {
		t.Fatal("round must not be recorded on bad signature")
	}
}

func TestHandleWebhook_EmptySecretRejects(t *testing.T) {
	r, _ := newHarness(t, nil)
	w := post(r, prBody, map[string]string{
		"X-Hub-Signature-256": sign([]byte(""), prBody),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "del-y",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when secret unset", w.Code)
	}
}

func TestHandleWebhook_NonPREventNoRound(t *testing.T) {
	secret := []byte("s3cr3t")
	r, st := newHarness(t, secret)
	ping := []byte(`{"zen":"x","repository":{"name":"caw","owner":{"login":"ravencloak-org"}}}`)
	w := post(r, ping, map[string]string{
		"X-Hub-Signature-256": sign(secret, ping),
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "del-ping",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if exists, _ := st.RoundExists("ravencloak-org", "caw", 0, ""); exists {
		t.Fatal("no round should be recorded for a non-PR event")
	}
}

func TestHandleWebhook_MalformedJSON(t *testing.T) {
	secret := []byte("s3cr3t")
	r, _ := newHarness(t, secret)
	bad := []byte(`{not json`)
	w := post(r, bad, map[string]string{
		"X-Hub-Signature-256": sign(secret, bad),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "del-bad",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleWebhook_StoreErrorReturns500(t *testing.T) {
	secret := []byte("s3cr3t")
	st, err := store.Open(filepath.Join(t.TempDir(), "err.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = st.Close() // force the dedupe query to fail

	r := gin.New()
	r.POST("/webhooks/github", New(st, secret).HandleWebhook)

	w := post(r, prBody, map[string]string{
		"X-Hub-Signature-256": sign(secret, prBody),
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "del-err",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
