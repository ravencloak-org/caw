package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

// leaseHarness builds a gin engine with the POST /leases/:owner/:repo/:number route.
// The auth middleware is bypassed: we inject installation_id directly into gin context
// so we can test the handler logic in isolation without real token crypto.
func leaseHarness(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := New(st, []byte("secret"), nil)
	r := gin.New()
	// inject middleware that sets installation_id like auth.Required would.
	injectID := func(id string) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.ContextInstallationID, id)
			c.Next()
		}
	}
	// We use a wrapper route per test via a factory; here we expose a builder.
	// For simplicity wire a default route using "inst-A"; individual tests override
	// via direct handler invocation where needed.
	r.POST("/leases/:owner/:repo/:number", injectID("inst-A"), h.HandleAcquireLease)
	return r, st
}

func doLeaseRequest(r *gin.Engine, owner, repo, number string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/leases/"+owner+"/"+repo+"/"+number, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type leaseResp struct {
	Granted         bool   `json:"granted"`
	Holder          string `json:"holder"`
	ExpiresAt       int64  `json:"expires_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	AcquiredAt      int64  `json:"acquired_at"`
}

func decodeLeaseResp(t *testing.T, w *httptest.ResponseRecorder) leaseResp {
	t.Helper()
	var resp leaseResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// --- Tests ---

func TestHandleAcquireLease_FirstGrantReturns200(t *testing.T) {
	r, _ := leaseHarness(t)
	w := doLeaseRequest(r, "org", "repo", "1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	resp := decodeLeaseResp(t, w)
	if !resp.Granted {
		t.Fatalf("expected granted=true")
	}
	if resp.Holder != "inst-A" {
		t.Fatalf("holder = %q, want inst-A", resp.Holder)
	}
	if resp.ExpiresAt <= resp.AcquiredAt {
		t.Fatalf("expires_at %d <= acquired_at %d", resp.ExpiresAt, resp.AcquiredAt)
	}
}

func TestHandleAcquireLease_SameHolderRetains200(t *testing.T) {
	r, _ := leaseHarness(t)
	// First grant.
	w1 := doLeaseRequest(r, "org", "repo", "2")
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status %d", w1.Code)
	}
	// Same holder requests again.
	w2 := doLeaseRequest(r, "org", "repo", "2")
	if w2.Code != http.StatusOK {
		t.Fatalf("second request (same holder): status %d, want 200", w2.Code)
	}
}

func TestHandleAcquireLease_SecondHolderDeniedWith409(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "deny.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := New(st, []byte("secret"), nil)
	r := gin.New()

	// inst-A acquires first.
	r.POST("/leases/:owner/:repo/:number", func(c *gin.Context) {
		c.Set(auth.ContextInstallationID, "inst-A")
		h.HandleAcquireLease(c)
	})
	w1 := doLeaseRequest(r, "org", "repo", "3")
	if w1.Code != http.StatusOK {
		t.Fatalf("inst-A first: status %d", w1.Code)
	}

	// inst-B tries to steal.
	r2 := gin.New()
	r2.POST("/leases/:owner/:repo/:number", func(c *gin.Context) {
		c.Set(auth.ContextInstallationID, "inst-B")
		h.HandleAcquireLease(c)
	})
	w2 := doLeaseRequest(r2, "org", "repo", "3")
	if w2.Code != http.StatusConflict {
		t.Fatalf("inst-B steal: status = %d, want 409; body=%q", w2.Code, w2.Body.String())
	}
	resp := decodeLeaseResp(t, w2)
	if resp.Granted {
		t.Fatalf("expected granted=false for competing holder")
	}
	if resp.Holder != "inst-A" {
		t.Fatalf("holder = %q, want inst-A (current owner)", resp.Holder)
	}
}

func TestHandleAcquireLease_MissingInstallationID_401(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "noauth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := New(st, []byte("secret"), nil)
	r := gin.New()
	// No middleware sets installation_id.
	r.POST("/leases/:owner/:repo/:number", h.HandleAcquireLease)

	w := doLeaseRequest(r, "org", "repo", "4")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestHandleAcquireLease_InvalidNumber_400(t *testing.T) {
	r, _ := leaseHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/leases/org/repo/notanumber", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid number", w.Code)
	}
}

func TestHandleAcquireLease_ExpiredLeaseReacquiredByDifferentHolder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "expired.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := New(st, []byte("secret"), nil)

	// inst-A acquires a lease with a negative duration, so it is already expired
	// the moment it is written (ExpiresAt = now-1).
	a1, err := st.AcquireLease("org", "repo", 7, "inst-A", -1)
	if err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}
	if !a1.Granted {
		t.Fatalf("expected inst-A to be granted the initial (expired) lease")
	}

	// inst-B now requests the lease through the handler. Because inst-A's lease is
	// expired, the grant must succeed and inst-B becomes the holder with a 200.
	r := gin.New()
	r.POST("/leases/:owner/:repo/:number", func(c *gin.Context) {
		c.Set(auth.ContextInstallationID, "inst-B")
		h.HandleAcquireLease(c)
	})
	w := doLeaseRequest(r, "org", "repo", "7")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (expired lease re-acquirable); body=%q", w.Code, w.Body.String())
	}
	resp := decodeLeaseResp(t, w)
	if !resp.Granted {
		t.Fatalf("expected granted=true for new holder over expired lease")
	}
	if resp.Holder != "inst-B" {
		t.Fatalf("holder = %q, want inst-B (new holder)", resp.Holder)
	}
}

func TestHandleAcquireLease_IsolatedByPR(t *testing.T) {
	r, _ := leaseHarness(t)
	// PR 5 and PR 6 are independent — both should be granted.
	w1 := doLeaseRequest(r, "org", "repo", "5")
	w2 := doLeaseRequest(r, "org", "repo", "6")

	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("status PR5=%d PR6=%d, want 200 both", w1.Code, w2.Code)
	}
}
