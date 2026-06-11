package ghclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestEnableAutoMerge(t *testing.T) {
	var method, path, auth, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := New(srv.URL, StaticToken("tok")).EnableAutoMerge(context.Background(), "o", "r", 1, "squash"); err != nil {
		t.Fatalf("EnableAutoMerge: %v", err)
	}
	if method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", method)
	}
	if path != "/repos/o/r/pulls/1" {
		t.Errorf("path = %s, want /repos/o/r/pulls/1", path)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", auth)
	}
	if !strings.Contains(body, `"merge_method":"squash"`) {
		t.Errorf("body = %s, want squash merge_method", body)
	}
}

func TestEnableAutoMergeNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	// Empty merge method defaults to "merge"; nil token source sends no auth.
	if err := New(srv.URL, nil).EnableAutoMerge(context.Background(), "o", "r", 1, ""); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestEnableAutoMergeTokenSourceError(t *testing.T) {
	errSrc := func(context.Context, string, string) (string, error) {
		return "", errors.New("boom")
	}
	if err := New("http://127.0.0.1:0", errSrc).EnableAutoMerge(context.Background(), "o", "r", 1, ""); err == nil {
		t.Fatal("expected error when the token source fails")
	}
}

func TestNewDefaultsBaseURL(t *testing.T) {
	if got := New("", nil).baseURL; got != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", got, defaultBaseURL)
	}
}
