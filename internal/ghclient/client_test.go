package ghclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPullMergeability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_, _ = w.Write([]byte(`{"state":"open","mergeable":true,"mergeable_state":"behind"}`))
	}))
	defer srv.Close()

	ps, err := New(srv.URL, StaticToken("tok")).PullMergeability(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("PullMergeability: %v", err)
	}
	if ps.State != "open" || ps.MergeableState != "behind" || ps.Mergeable == nil || !*ps.Mergeable {
		t.Fatalf("PullState = %+v", ps)
	}
}

func TestPullMergeabilityNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, nil).PullMergeability(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestPullMergeabilityTokenSourceError(t *testing.T) {
	errSrc := func(context.Context, string, string) (string, error) {
		return "", errors.New("boom")
	}
	if _, err := New("http://127.0.0.1:0", errSrc).PullMergeability(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("expected error when the token source fails")
	}
}
