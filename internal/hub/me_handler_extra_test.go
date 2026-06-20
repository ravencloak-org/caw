package hub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ravencloak-org/caw/internal/store"
)

// Fills the small coverage gaps the main me_handler_test.go leaves on
// NewMeHandler (nil-nowFn default), HandleMeTokens (no-token user),
// HandleMeRecover (zero-row revoke still flushes cache), HandleMeTokenRevoke
// (empty :id).

func TestNewMeHandler_NilNowFnDefaultsToWallClock(t *testing.T) {
	// nowFn=nil ⇒ handler populates a default clock from time.Now. Verify
	// the constructor accepts nil and produces a working handler whose
	// `now` field returns a non-zero Unix timestamp.
	h := NewMeHandler(nil, nil, nil)
	if h == nil {
		t.Fatal("NewMeHandler returned nil")
	}
	if got := h.now(); got <= 0 {
		t.Errorf("h.now() = %d, want positive Unix timestamp", got)
	}
}

func TestMeTokens_UserWithNoTokensReturnsEmptyList(t *testing.T) {
	h := newMeHarness(t)
	// Sibling user's token exists; ours doesn't. Listing for our user must
	// return [] (not a 404, not a null body).
	seedToken(t, h.st, store.Token{
		ID:              "other",
		Hash:            "hash-other",
		InstallationID:  "inst-2",
		Org:             "beta",
		DeviceLabel:     "other-device",
		GitHubUserID:    uid(9999),
		GitHubUserLogin: "bob",
	})
	w := doReq(h.r, http.MethodGet, "/me/tokens?user=7777&login=alice")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "[]") {
		t.Errorf("expected empty array body, got %s", body)
	}
}

func TestMeRecover_UserWithNoTokensStillReturns204AndFlushes(t *testing.T) {
	h := newMeHarness(t)
	// No tokens for this user. Recover must still 204 + flush cache —
	// defense in depth against a race with a fresh mint.
	w := doReq(h.r, http.MethodPost, "/me/recover?user=7777&login=alice")
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}
	if len(h.flusher.flushed) != 1 || h.flusher.flushed[0] != 7777 {
		t.Errorf("FlushUser calls = %v, want [7777]", h.flusher.flushed)
	}
}

func TestMeTokenRevoke_EmptyIDReturns404(t *testing.T) {
	h := newMeHarness(t)
	// /me/tokens/ (trailing slash, no id) — gin returns 404 from the router
	// because no route matches. Confirms the user-facing behavior when an
	// id is missing.
	w := doReq(h.r, http.MethodDelete, "/me/tokens/?user=7777&login=alice")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
