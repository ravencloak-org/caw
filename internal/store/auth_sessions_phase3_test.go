package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tests for the Phase 3 additions to auth_sessions.go: SetSessionUser,
// SetSessionPendingBundle, GetSessionByDeviceCode, GetSessionByUserCode,
// plus AnyAppSlug. Mirrors the tokens_v2_test.go shape used by Phase 1.

func TestSetSessionUser_PersistsBoth(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("phase3-user-1", "loopback")
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	if err := s.SetSessionUser(a.ID, 12345, "alice"); err != nil {
		t.Fatalf("SetSessionUser: %v", err)
	}
	got, ok, err := s.GetAuthSession(a.ID)
	if err != nil || !ok {
		t.Fatalf("GetAuthSession: ok=%v err=%v", ok, err)
	}
	if got.GitHubUserID == nil || *got.GitHubUserID != 12345 {
		t.Errorf("GitHubUserID = %v, want 12345", got.GitHubUserID)
	}
	if got.GitHubUserLogin != "alice" {
		t.Errorf("GitHubUserLogin = %q, want alice", got.GitHubUserLogin)
	}
}

func TestSetSessionUser_RequiresID(t *testing.T) {
	s := newTestStore(t)
	err := s.SetSessionUser("", 1, "x")
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Errorf("want id required error, got %v", err)
	}
}

func TestSetSessionUser_NoRowFails(t *testing.T) {
	s := newTestStore(t)
	err := s.SetSessionUser("ghost-id-does-not-exist", 1, "x")
	if err == nil || !strings.Contains(err.Error(), "no row") {
		t.Errorf("want no-row error, got %v", err)
	}
}

func TestSetSessionPendingBundle_DeliveredState(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("phase3-bundle-1", "device")
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	bundle := `{"github_user_id":42,"tokens":[]}`
	if err := s.SetSessionPendingBundle(a.ID, bundle); err != nil {
		t.Fatalf("SetSessionPendingBundle: %v", err)
	}
	got, _, _ := s.GetAuthSession(a.ID)
	if got.PendingBundleJSON != bundle {
		t.Errorf("PendingBundleJSON = %q, want %q", got.PendingBundleJSON, bundle)
	}
	if got.State != "delivered" {
		t.Errorf("State = %q, want delivered", got.State)
	}
	// Bundle must round-trip JSON.
	var parsed struct {
		GitHubUserID int64 `json:"github_user_id"`
	}
	if err := json.Unmarshal([]byte(got.PendingBundleJSON), &parsed); err != nil {
		t.Errorf("unmarshal bundle: %v", err)
	}
	if parsed.GitHubUserID != 42 {
		t.Errorf("decoded github_user_id = %d, want 42", parsed.GitHubUserID)
	}
}

func TestSetSessionPendingBundle_RequiresInputs(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSessionPendingBundle("", "x"); err == nil {
		t.Errorf("empty id accepted")
	}
	if err := s.SetSessionPendingBundle("id", ""); err == nil {
		t.Errorf("empty bundle accepted")
	}
	if err := s.SetSessionPendingBundle("never-existed", "{}"); err == nil {
		t.Errorf("non-existent id accepted")
	}
}

func TestGetSessionByDeviceCode_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("phase3-devlookup", "device")
	a.DeviceCode = "DC-deadbeefcafe"
	a.UserCode = "WDJB-MJHT"
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	got, ok, err := s.GetSessionByDeviceCode("DC-deadbeefcafe")
	if err != nil || !ok {
		t.Fatalf("by device_code: ok=%v err=%v", ok, err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
	if _, ok, _ := s.GetSessionByDeviceCode("ghost"); ok {
		t.Errorf("ghost device_code matched")
	}
	if _, ok, _ := s.GetSessionByDeviceCode(""); ok {
		t.Errorf("empty device_code matched")
	}
}

func TestGetSessionByUserCode_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("phase3-userlookup", "device")
	a.DeviceCode = "DC-zzz"
	a.UserCode = "AAAA-BBBB"
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	got, ok, err := s.GetSessionByUserCode("AAAA-BBBB")
	if err != nil || !ok {
		t.Fatalf("by user_code: ok=%v err=%v", ok, err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
	if _, ok, _ := s.GetSessionByUserCode("ZZZZ-ZZZZ"); ok {
		t.Errorf("ghost user_code matched")
	}
}

func TestAnyAppSlug_EmptyByDefault(t *testing.T) {
	s := newTestStore(t)
	slug, err := s.AnyAppSlug()
	if err != nil {
		t.Fatalf("AnyAppSlug: %v", err)
	}
	if slug != "" {
		t.Errorf("AnyAppSlug = %q, want empty", slug)
	}
}

func TestAnyAppSlug_FindsFirstNonEmpty(t *testing.T) {
	s := newTestStore(t)
	// Two installations: first with empty slug, second populated. AnyAppSlug
	// must return the populated one regardless of insertion order.
	if err := s.UpsertInstallation("inst-empty-slug", "acme"); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := s.UpsertInstallation("inst-with-slug", "bcorp"); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := s.UpdateInstallationAppSlug("inst-with-slug", "caw-ravencloak"); err != nil {
		t.Fatalf("UpdateInstallationAppSlug: %v", err)
	}
	slug, err := s.AnyAppSlug()
	if err != nil {
		t.Fatalf("AnyAppSlug: %v", err)
	}
	if slug != "caw-ravencloak" {
		t.Errorf("AnyAppSlug = %q, want caw-ravencloak", slug)
	}
}
