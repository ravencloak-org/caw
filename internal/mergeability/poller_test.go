package mergeability

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ravencloak-org/caw/internal/ghclient"
)

func TestPollerClassifies(t *testing.T) {
	cases := []struct {
		state    string
		wantBody string
		wantSev  string
	}{
		{"clean", "clean", ""},
		{"behind", "behind base", "MINOR"},
		{"dirty", "conflicts", "MAJOR"},
		{"blocked", "blocked", "MINOR"},
		{"weird", "weird", ""},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"mergeable_state":%q}`, tc.state)
			}))
			defer srv.Close()

			sig, ok, err := New(ghclient.New(srv.URL, "")).Mergeability("o", "r", 1, "sha")
			if err != nil || !ok {
				t.Fatalf("err=%v ok=%v", err, ok)
			}
			if sig.SignalType != "mergeability" || sig.Source != "poll" || sig.ExternalID != "mergeability" {
				t.Fatalf("signal identity wrong: %+v", sig)
			}
			if sig.Body != tc.wantBody || sig.Severity != tc.wantSev {
				t.Fatalf("state %q => body=%q sev=%q, want body=%q sev=%q",
					tc.state, sig.Body, sig.Severity, tc.wantBody, tc.wantSev)
			}
			if sig.Owner != "o" || sig.Repo != "r" || sig.Number != 1 || sig.SHA != "sha" {
				t.Fatalf("round fields wrong: %+v", sig)
			}
		})
	}
}

func TestPollerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, ok, err := New(ghclient.New(srv.URL, "")).Mergeability("o", "r", 1, "sha"); ok || err == nil {
		t.Fatalf("expected error/!ok on 404, got ok=%v err=%v", ok, err)
	}
}
