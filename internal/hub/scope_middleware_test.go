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

// scopeHarness creates a store seeded with two installations and their repos,
// and builds a gin engine wired with RequireRepoScope. It mirrors the
// production route setup: authMW first (replaced here with injectID), then
// RequireRepoScope, then the handler.
//
// Installations:
//
//	inst-A → a/x
//	inst-B → b/y
func scopeHarness(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed installation A with repo a/x.
	if err := st.UpsertInstallation("inst-A", "org-a"); err != nil {
		t.Fatalf("upsert inst-A: %v", err)
	}
	if err := st.AddInstallationRepo("inst-A", "a/x"); err != nil {
		t.Fatalf("add repo a/x to inst-A: %v", err)
	}

	// Seed installation B with repo b/y.
	if err := st.UpsertInstallation("inst-B", "org-b"); err != nil {
		t.Fatalf("upsert inst-B: %v", err)
	}
	if err := st.AddInstallationRepo("inst-B", "b/y"); err != nil {
		t.Fatalf("add repo b/y to inst-B: %v", err)
	}

	h := New(st, []byte("secret"), nil)
	scopeMW := RequireRepoScope(st)

	r := gin.New()

	// injectID simulates what auth.Required does without needing real tokens.
	injectID := func(id string) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.ContextInstallationID, id)
			c.Next()
		}
	}

	// Wire the three routes using a query-param "caller" to select installation.
	// The GET /pending route does NOT go through scopeMW (it uses the handler's own check).
	r.GET("/pending/:caller", func(c *gin.Context) {
		c.Set(auth.ContextInstallationID, c.Param("caller"))
		h.HandlePending(c)
	})
	r.POST("/leases/:owner/:repo/:number", injectID("inst-A"), scopeMW, h.HandleAcquireLease)
	r.GET("/sse/:owner/:repo/:number", injectID("inst-A"), scopeMW, func(c *gin.Context) {
		c.String(http.StatusOK, "sse-ok")
	})

	// Routes with inst-B identity.
	r.POST("/leases-b/:owner/:repo/:number", injectID("inst-B"), scopeMW, h.HandleAcquireLease)
	r.GET("/sse-b/:owner/:repo/:number", injectID("inst-B"), scopeMW, func(c *gin.Context) {
		c.String(http.StatusOK, "sse-ok")
	})

	// Routes with setup identity.
	r.POST("/leases-setup/:owner/:repo/:number", injectID("setup"), scopeMW, h.HandleAcquireLease)
	r.GET("/sse-setup/:owner/:repo/:number", injectID("setup"), scopeMW, func(c *gin.Context) {
		c.String(http.StatusOK, "sse-ok")
	})

	return r, st
}

// --- RequireRepoScope tests ---

// TestRequireRepoScope_AllowsAssociatedRepo: inst-A accessing a/x must pass through.
func TestRequireRepoScope_AllowsAssociatedRepo(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/leases/a/x/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 200 (lease granted) or 409 (conflict) both mean the scope check passed.
	if w.Code == http.StatusForbidden {
		t.Fatalf("inst-A accessing a/x got 403, want pass-through; body=%q", w.Body.String())
	}
}

// TestRequireRepoScope_BlocksCrossTenantAccess: inst-A accessing b/y must get 403.
func TestRequireRepoScope_BlocksCrossTenantAccess(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/leases/b/y/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("inst-A accessing b/y: status = %d, want 403 (IDOR block); body=%q",
			w.Code, w.Body.String())
	}
}

// TestRequireRepoScope_InstBAllowedOnOwnRepo: inst-B accessing b/y must pass through.
func TestRequireRepoScope_InstBAllowedOnOwnRepo(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/leases-b/b/y/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden {
		t.Fatalf("inst-B accessing b/y got 403, want pass-through; body=%q", w.Body.String())
	}
}

// TestRequireRepoScope_InstBBlockedOnAX: inst-B accessing a/x must get 403.
func TestRequireRepoScope_InstBBlockedOnAX(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/leases-b/a/x/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("inst-B accessing a/x: status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

// TestRequireRepoScope_SetupTokenBlocked: "setup" identity must be 403 on /leases.
func TestRequireRepoScope_SetupTokenBlockedOnLease(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/leases-setup/a/x/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("setup token on /leases: status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

// TestRequireRepoScope_SetupTokenBlockedOnSSE: "setup" identity must be 403 on /sse.
func TestRequireRepoScope_SetupTokenBlockedOnSSE(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/sse-setup/a/x/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("setup token on /sse: status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

// TestRequireRepoScope_SSEAllowsAssociatedRepo: inst-A can subscribe to a/x.
func TestRequireRepoScope_SSEAllowsAssociatedRepo(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/sse/a/x/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("inst-A subscribing to a/x: status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// TestRequireRepoScope_SSEBlocksCrossTenant: inst-A must not subscribe to b/y.
func TestRequireRepoScope_SSEBlocksCrossTenant(t *testing.T) {
	r, _ := scopeHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/sse/b/y/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("inst-A subscribing to b/y: status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

// --- HandlePending scoped tests ---

// pendingResp mirrors the JSON structure returned by HandlePending.
type pendingResp struct {
	Items []store.PendingItem `json:"items"`
}

func decodePending(t *testing.T, w *httptest.ResponseRecorder) pendingResp {
	t.Helper()
	var resp pendingResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode pending response: %v", err)
	}
	return resp
}

// TestHandlePending_ScopedToInstallation: each installation sees only its own items.
func TestHandlePending_ScopedToInstallation(t *testing.T) {
	r, st := scopeHarness(t)

	// Insert a round for a/x (owned by inst-A).
	if err := st.RecordRound("a", "x", 10, "sha-a"); err != nil {
		t.Fatalf("record round a/x: %v", err)
	}
	// Insert a round for b/y (owned by inst-B).
	if err := st.RecordRound("b", "y", 20, "sha-b"); err != nil {
		t.Fatalf("record round b/y: %v", err)
	}

	// inst-A's pending must only contain a/x items.
	wA := httptest.NewRecorder()
	r.ServeHTTP(wA, httptest.NewRequest(http.MethodGet, "/pending/inst-A", nil))
	if wA.Code != http.StatusOK {
		t.Fatalf("inst-A /pending: status = %d, want 200; body=%q", wA.Code, wA.Body.String())
	}
	respA := decodePending(t, wA)
	for _, item := range respA.Items {
		if item.Owner != "a" || item.Repo != "x" {
			t.Errorf("inst-A pending contains unexpected item %s/%s (cross-tenant leak)", item.Owner, item.Repo)
		}
	}

	// inst-B's pending must only contain b/y items.
	wB := httptest.NewRecorder()
	r.ServeHTTP(wB, httptest.NewRequest(http.MethodGet, "/pending/inst-B", nil))
	if wB.Code != http.StatusOK {
		t.Fatalf("inst-B /pending: status = %d, want 200; body=%q", wB.Code, wB.Body.String())
	}
	respB := decodePending(t, wB)
	for _, item := range respB.Items {
		if item.Owner != "b" || item.Repo != "y" {
			t.Errorf("inst-B pending contains unexpected item %s/%s (cross-tenant leak)", item.Owner, item.Repo)
		}
	}
}

// TestHandlePending_SetupTokenReturns403: the bootstrap "setup" token must be rejected.
func TestHandlePending_SetupTokenReturns403(t *testing.T) {
	r, _ := scopeHarness(t)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pending/setup", nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("setup token on /pending: status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

// TestHandleAcquireLease_SetupTokenDefenseInDepth: even without RequireRepoScope in the
// chain, HandleAcquireLease must reject the "setup" sentinel at the handler level.
func TestHandleAcquireLease_SetupTokenDefenseInDepth(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "did.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := New(st, []byte("secret"), nil)

	// Wire WITHOUT RequireRepoScope — only the in-handler guard applies.
	r := gin.New()
	r.POST("/leases/:owner/:repo/:number", func(c *gin.Context) {
		c.Set(auth.ContextInstallationID, "setup")
		h.HandleAcquireLease(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/leases/a/x/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("setup token on /leases (no scope MW): status = %d, want 403; body=%q",
			w.Code, w.Body.String())
	}
}
