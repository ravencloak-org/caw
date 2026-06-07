package settle

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ravencloak-org/caw/internal/store"
)

type fakePub struct {
	mu       sync.Mutex
	count    int // value Publish returns (simulated subscriber count)
	messages [][]byte
}

func (f *fakePub) Publish(_ string, msg []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msg)
	return f.count
}

func (f *fakePub) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakePub) last() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[len(f.messages)-1]
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seed(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.AddSignal(store.Signal{
		Owner: "o", Repo: "r", Number: 1, SHA: "sha",
		SignalType: "checks", Source: "CI", ExternalID: "1", Body: "fail",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestSettleFiresAfterGrace(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	pub := &fakePub{count: 1}
	e := New(st, pub, 20*time.Millisecond)

	e.Touch("o", "r", 1, "sha")
	if pub.n() != 0 { // timers never fire before their deadline
		t.Fatal("settled before grace elapsed")
	}
	time.Sleep(120 * time.Millisecond)
	if pub.n() != 1 {
		t.Fatalf("messages = %d, want 1", pub.n())
	}
}

func TestReSettleReArmsTimer(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	pub := &fakePub{count: 1}
	e := New(st, pub, 50*time.Millisecond)

	e.Touch("o", "r", 1, "sha") // deadline ~t+50
	time.Sleep(30 * time.Millisecond)
	e.Touch("o", "r", 1, "sha") // re-arm: deadline ~t+80
	time.Sleep(35 * time.Millisecond)
	if pub.n() != 0 { // ~t+65 < t+80: cannot have fired yet
		t.Fatalf("re-settle fired early: %d", pub.n())
	}
	time.Sleep(120 * time.Millisecond)
	if pub.n() != 1 {
		t.Fatalf("messages = %d, want 1", pub.n())
	}
}

func TestOrphanStoresPending(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	pub := &fakePub{count: 0} // no subscribers
	e := New(st, pub, time.Millisecond)

	e.FireNow("o", "r", 1, "sha")

	items, err := st.ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(items) != 1 || items[0].SignalType != "checks" {
		t.Fatalf("pending = %+v, want one checks item", items)
	}
}

func TestSeqIncrementsAcrossSettles(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	pub := &fakePub{count: 1}
	e := New(st, pub, time.Millisecond)

	e.FireNow("o", "r", 1, "sha")
	e.FireNow("o", "r", 1, "sha")
	if pub.n() != 2 {
		t.Fatalf("messages = %d, want 2", pub.n())
	}
	var got struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(pub.last(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Seq != 2 {
		t.Fatalf("seq = %d, want 2", got.Seq)
	}
}

func TestPRKey(t *testing.T) {
	if got := PRKey("o", "r", 7); got != "o/r#7" {
		t.Fatalf("PRKey = %q", got)
	}
}

type fakePoller struct {
	sig store.Signal
	ok  bool
	err error
}

func (f fakePoller) Mergeability(owner, repo string, number int, sha string) (store.Signal, bool, error) {
	f.sig.Owner, f.sig.Repo, f.sig.Number, f.sig.SHA = owner, repo, number, sha
	return f.sig, f.ok, f.err
}

func TestSettleWithPollerAddsMergeabilitySignal(t *testing.T) {
	st := newStore(t)
	pub := &fakePub{count: 1}
	fp := fakePoller{
		sig: store.Signal{SignalType: "mergeability", Source: "poll", ExternalID: "mergeability", Body: "behind base"},
		ok:  true,
	}
	e := New(st, pub, time.Millisecond, WithPoller(fp))

	e.FireNow("o", "r", 1, "sha")

	sigs, err := st.SignalsForRound("o", "r", 1, "sha")
	if err != nil {
		t.Fatalf("SignalsForRound: %v", err)
	}
	found := false
	for _, s := range sigs {
		if s.SignalType == "mergeability" && s.Body == "behind base" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mergeability signal not added by poller; got %+v", sigs)
	}
}

func TestSettlePollerErrorIsNonFatal(t *testing.T) {
	st := newStore(t)
	pub := &fakePub{count: 1}
	e := New(st, pub, time.Millisecond, WithPoller(fakePoller{err: errTest}))

	e.FireNow("o", "r", 1, "sha") // must not panic; just logs

	sigs, _ := st.SignalsForRound("o", "r", 1, "sha")
	for _, s := range sigs {
		if s.SignalType == "mergeability" {
			t.Fatal("no mergeability signal should be stored on poll error")
		}
	}
}

var errTest = errTestType("poll failed")

type errTestType string

func (e errTestType) Error() string { return string(e) }
