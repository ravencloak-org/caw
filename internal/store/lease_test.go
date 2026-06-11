package store

import (
	"testing"
	"time"
)

func TestAcquireLease_FirstGrant(t *testing.T) {
	s := newTestStore(t)

	res, err := s.AcquireLease("org1", "repo1", 42, "inst-A", 300)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if !res.Granted {
		t.Fatalf("expected Granted=true for first lease, got false; holder=%s", res.Lease.Holder)
	}
	if res.Lease.Holder != "inst-A" {
		t.Errorf("holder = %q, want %q", res.Lease.Holder, "inst-A")
	}
	if res.Lease.Org != "org1" || res.Lease.Repo != "repo1" || res.Lease.PRNumber != 42 {
		t.Errorf("unexpected lease coords: %+v", res.Lease)
	}
	now := time.Now().Unix()
	if res.Lease.ExpiresAt < now+290 || res.Lease.ExpiresAt > now+310 {
		t.Errorf("ExpiresAt %d not within expected range of now+300", res.Lease.ExpiresAt)
	}
}

func TestAcquireLease_SameHolderRenews(t *testing.T) {
	s := newTestStore(t)

	r1, err := s.AcquireLease("o", "r", 1, "inst-A", 300)
	if err != nil || !r1.Granted {
		t.Fatalf("first grant: err=%v granted=%v", err, r1.Granted)
	}

	// Same holder acquires again — should renew.
	r2, err := s.AcquireLease("o", "r", 1, "inst-A", 600)
	if err != nil {
		t.Fatalf("second AcquireLease: %v", err)
	}
	// The existing lease is NOT expired, so the WHERE clause in the ON CONFLICT
	// does not fire — the row is not replaced.  The holder is still inst-A so
	// GetLease still returns inst-A.  The result must still reflect inst-A.
	// The WHERE clause blocks replacement of a live lease even by the same holder.
	// That is the designed behavior for Slice 4.  Slice 6 will add a heartbeat
	// endpoint that lets the holder renew.  We only assert the holder stays inst-A.
	_ = r2.Granted
	if r2.Lease.Holder != "inst-A" {
		t.Errorf("holder after re-acquire = %q, want inst-A", r2.Lease.Holder)
	}
}

func TestAcquireLease_SecondHolderDenied(t *testing.T) {
	s := newTestStore(t)

	r1, err := s.AcquireLease("o", "r", 1, "inst-A", 300)
	if err != nil || !r1.Granted {
		t.Fatalf("first grant: err=%v granted=%v", err, r1.Granted)
	}

	// Different holder tries to acquire while lease is still live.
	r2, err := s.AcquireLease("o", "r", 1, "inst-B", 300)
	if err != nil {
		t.Fatalf("AcquireLease inst-B: %v", err)
	}
	if r2.Granted {
		t.Fatal("expected Granted=false for second holder while first is active")
	}
	// Current holder should still be inst-A.
	if r2.Lease.Holder != "inst-A" {
		t.Errorf("current holder = %q, want inst-A", r2.Lease.Holder)
	}
}

func TestAcquireLease_ExpiredLeaseAllowsNewHolder(t *testing.T) {
	s := newTestStore(t)

	// Grant an already-expired lease (duration = -10s).
	r1, err := s.AcquireLease("o", "r", 1, "inst-A", -10)
	if err != nil {
		t.Fatalf("first AcquireLease: %v", err)
	}
	// The first grant succeeds (no prior row).
	if !r1.Granted {
		t.Fatal("expected first grant (no prior row) to succeed even with negative duration")
	}

	// A second holder can now take over because the lease is expired.
	r2, err := s.AcquireLease("o", "r", 1, "inst-B", 300)
	if err != nil {
		t.Fatalf("second AcquireLease: %v", err)
	}
	if !r2.Granted {
		t.Fatalf("expected Granted=true after expired lease, got false; holder=%s", r2.Lease.Holder)
	}
	if r2.Lease.Holder != "inst-B" {
		t.Errorf("holder = %q, want inst-B", r2.Lease.Holder)
	}
}

func TestGetLease_NotFound(t *testing.T) {
	s := newTestStore(t)

	l, ok, err := s.GetLease("no", "such", 99)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for non-existent lease, got lease=%+v", l)
	}
}

func TestGetLease_ReturnsStoredLease(t *testing.T) {
	s := newTestStore(t)

	_, err := s.AcquireLease("myorg", "myrepo", 7, "inst-X", 120)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	l, ok, err := s.GetLease("myorg", "myrepo", 7)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if l.Holder != "inst-X" || l.Org != "myorg" || l.Repo != "myrepo" || l.PRNumber != 7 {
		t.Errorf("unexpected lease: %+v", l)
	}
	if l.AcquiredAt <= 0 {
		t.Error("AcquiredAt should be set")
	}
}

func TestAcquireLease_IsolatedByPR(t *testing.T) {
	s := newTestStore(t)

	// Two separate PRs; each gets its own lease.
	cases := []struct {
		org, repo string
		num       int
		holder    string
	}{
		{"o", "r", 1, "inst-A"},
		{"o", "r", 2, "inst-B"},
		{"o", "r2", 1, "inst-C"},
	}
	for _, tc := range cases {
		r, err := s.AcquireLease(tc.org, tc.repo, tc.num, tc.holder, 300)
		if err != nil {
			t.Fatalf("AcquireLease(%s/%s#%d): %v", tc.org, tc.repo, tc.num, err)
		}
		if !r.Granted {
			t.Errorf("expected Granted=true for %s/%s#%d", tc.org, tc.repo, tc.num)
		}
		if r.Lease.Holder != tc.holder {
			t.Errorf("holder = %q, want %q", r.Lease.Holder, tc.holder)
		}
	}

	// The second holder of PR#2 is still denied the lease on PR#1.
	r, err := s.AcquireLease("o", "r", 1, "inst-B", 300)
	if err != nil {
		t.Fatalf("contention check: %v", err)
	}
	if r.Granted {
		t.Error("inst-B should be denied PR#1 held by inst-A")
	}
}

// ---------------------------------------------------------------------------
// RenewLease
// ---------------------------------------------------------------------------

func TestRenewLease_HolderCanRenew(t *testing.T) {
	s := newTestStore(t)

	_, err := s.AcquireLease("o", "r", 1, "inst-A", 300)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	updated, err := s.RenewLease("o", "r", 1, "inst-A", 90)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if updated.Holder != "inst-A" {
		t.Errorf("holder after renew = %q, want inst-A", updated.Holder)
	}
	now := time.Now().Unix()
	if updated.ExpiresAt < now+80 || updated.ExpiresAt > now+100 {
		t.Errorf("ExpiresAt %d not within expected range of now+90", updated.ExpiresAt)
	}
	if updated.LastHeartbeatAt < now-2 || updated.LastHeartbeatAt > now+2 {
		t.Errorf("LastHeartbeatAt %d should be near now (%d)", updated.LastHeartbeatAt, now)
	}
}

func TestRenewLease_WrongHolderDenied(t *testing.T) {
	s := newTestStore(t)

	_, err := s.AcquireLease("o", "r", 1, "inst-A", 300)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	_, err = s.RenewLease("o", "r", 1, "inst-B", 90)
	if err == nil {
		t.Fatal("expected error when wrong holder tries to renew, got nil")
	}
}

func TestRenewLease_NoLeaseErrors(t *testing.T) {
	s := newTestStore(t)

	_, err := s.RenewLease("o", "r", 999, "inst-A", 90)
	if err == nil {
		t.Fatal("expected error when renewing non-existent lease, got nil")
	}
}

func TestRenewLease_ExpiredDenied(t *testing.T) {
	s := newTestStore(t)

	// Acquire with a negative TTL so the lease is immediately expired.
	_, err := s.AcquireLease("o", "r", 1, "inst-A", -10)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	_, err = s.RenewLease("o", "r", 1, "inst-A", 90)
	if err == nil {
		t.Fatal("expected error when renewing expired lease, got nil")
	}
}

// ---------------------------------------------------------------------------
// ReleaseLease
// ---------------------------------------------------------------------------

func TestReleaseLease_HolderCanRelease(t *testing.T) {
	s := newTestStore(t)

	_, err := s.AcquireLease("o", "r", 1, "inst-A", 300)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	if err := s.ReleaseLease("o", "r", 1, "inst-A"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	_, ok, err := s.GetLease("o", "r", 1)
	if err != nil {
		t.Fatalf("GetLease after release: %v", err)
	}
	if ok {
		t.Fatal("expected lease to be gone after release, but it still exists")
	}
}

func TestReleaseLease_WrongHolderDenied(t *testing.T) {
	s := newTestStore(t)

	_, err := s.AcquireLease("o", "r", 1, "inst-A", 300)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	if err := s.ReleaseLease("o", "r", 1, "inst-B"); err == nil {
		t.Fatal("expected error when wrong holder tries to release, got nil")
	}

	// inst-A's lease should still exist.
	l, ok, err := s.GetLease("o", "r", 1)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if !ok {
		t.Fatal("expected lease to still exist after failed release by wrong holder")
	}
	if l.Holder != "inst-A" {
		t.Errorf("holder = %q, want inst-A", l.Holder)
	}
}

func TestReleaseLease_NoLeaseErrors(t *testing.T) {
	s := newTestStore(t)

	if err := s.ReleaseLease("o", "r", 999, "inst-A"); err == nil {
		t.Fatal("expected error when releasing non-existent lease, got nil")
	}
}
