package store

import (
	"strings"
	"testing"
	"time"
)

func sampleAuthSession(id, mode string) AuthSession {
	now := time.Now().Unix()
	return AuthSession{
		ID:                  id,
		HandshakeMode:       mode,
		CodeChallenge:       "challenge-" + id,
		CodeChallengeMethod: "S256",
		ClientLabel:         "test client",
		CreatedAt:           now,
		ExpiresAt:           now + 600,
	}
}

func TestInsertAuthSession_RequiresID(t *testing.T) {
	s := newTestStore(t)
	err := s.InsertAuthSession(AuthSession{HandshakeMode: "loopback"})
	if err == nil || !strings.Contains(err.Error(), "ID is required") {
		t.Fatalf("want ID required error, got %v", err)
	}
}

func TestInsertAuthSession_RequiresHandshakeMode(t *testing.T) {
	s := newTestStore(t)
	err := s.InsertAuthSession(AuthSession{ID: "abc"})
	if err == nil || !strings.Contains(err.Error(), "HandshakeMode is required") {
		t.Fatalf("want HandshakeMode required error, got %v", err)
	}
}

func TestInsertAuthSession_DefaultsState(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("abc-default-state", "loopback")
	a.State = "" // explicit: empty must default to "pending"
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	got, ok, err := s.GetAuthSession("abc-default-state")
	if err != nil || !ok {
		t.Fatalf("GetAuthSession: ok=%v err=%v", ok, err)
	}
	if got.State != "pending" {
		t.Errorf("State = %q, want pending", got.State)
	}
}

func TestInsertAndGetAuthSession_LoopbackRoundTrip(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("loop-1", "loopback")
	a.LoopbackRedirect = "http://127.0.0.1:54711/cb"
	userID := int64(42)
	a.GitHubUserID = &userID
	a.GitHubUserLogin = "alice"
	a.PendingBundleJSON = `{"tokens":[]}`
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	got, ok, err := s.GetAuthSession("loop-1")
	if err != nil || !ok {
		t.Fatalf("GetAuthSession: ok=%v err=%v", ok, err)
	}
	if got.ID != a.ID || got.HandshakeMode != "loopback" || got.LoopbackRedirect != a.LoopbackRedirect {
		t.Errorf("loopback fields not round-tripped: %+v", got)
	}
	if got.GitHubUserID == nil || *got.GitHubUserID != 42 || got.GitHubUserLogin != "alice" {
		t.Errorf("user binding not round-tripped: id=%v login=%q", got.GitHubUserID, got.GitHubUserLogin)
	}
	if got.PendingBundleJSON != a.PendingBundleJSON {
		t.Errorf("PendingBundleJSON not round-tripped: got %q want %q", got.PendingBundleJSON, a.PendingBundleJSON)
	}
}

func TestInsertAndGetAuthSession_DeviceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("dev-1", "device")
	a.DeviceCode = "device-code-xyz"
	a.UserCode = "WDJB-MJHT"
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	got, ok, err := s.GetAuthSession("dev-1")
	if err != nil || !ok {
		t.Fatalf("GetAuthSession: ok=%v err=%v", ok, err)
	}
	if got.HandshakeMode != "device" || got.DeviceCode != a.DeviceCode || got.UserCode != a.UserCode {
		t.Errorf("device fields not round-tripped: %+v", got)
	}
	if got.GitHubUserID != nil {
		t.Errorf("GitHubUserID = %v, want nil pre-OAuth", got.GitHubUserID)
	}
}

func TestGetAuthSession_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, ok, err := s.GetAuthSession("nope")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false; got %+v", got)
	}
}

func TestUpdateAuthSessionState(t *testing.T) {
	s := newTestStore(t)
	a := sampleAuthSession("transition-1", "loopback")
	if err := s.InsertAuthSession(a); err != nil {
		t.Fatalf("InsertAuthSession: %v", err)
	}
	if err := s.UpdateAuthSessionState("transition-1", "delivered"); err != nil {
		t.Fatalf("UpdateAuthSessionState: %v", err)
	}
	got, _, err := s.GetAuthSession("transition-1")
	if err != nil {
		t.Fatalf("GetAuthSession: %v", err)
	}
	if got.State != "delivered" {
		t.Errorf("State = %q, want delivered", got.State)
	}
}

func TestUpdateAuthSessionState_Validates(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateAuthSessionState("", "delivered"); err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Errorf("missing id: want id-required error, got %v", err)
	}
	if err := s.UpdateAuthSessionState("any", ""); err == nil || !strings.Contains(err.Error(), "state is required") {
		t.Errorf("missing state: want state-required error, got %v", err)
	}
}

func TestDeleteExpiredAuthSessions(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	live := sampleAuthSession("live", "loopback")
	live.ExpiresAt = now + 600
	expired := sampleAuthSession("expired", "loopback")
	expired.ExpiresAt = now - 1
	if err := s.InsertAuthSession(live); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	if err := s.InsertAuthSession(expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	n, err := s.DeleteExpiredAuthSessions(now)
	if err != nil {
		t.Fatalf("DeleteExpiredAuthSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("rows deleted = %d, want 1", n)
	}
	if _, ok, _ := s.GetAuthSession("expired"); ok {
		t.Error("expired row should be gone")
	}
	if _, ok, _ := s.GetAuthSession("live"); !ok {
		t.Error("live row should remain")
	}
}
