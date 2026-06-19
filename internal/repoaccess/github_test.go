package repoaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// staticInstallToken returns an InstallTokenSource that always yields tok.
func staticInstallToken(tok string) InstallTokenSource {
	return func(context.Context, string) (string, error) { return tok, nil }
}

// failingInstallToken returns an InstallTokenSource that always errors.
func failingInstallToken(err error) InstallTokenSource {
	return func(context.Context, string) (string, error) { return "", err }
}

// newStubGitHub builds an httptest server that returns status and (when 200)
// body, and exposes the captured request via gotReq for assertion.
func newStubGitHub(t *testing.T, status int, body string, gotReq *http.Request) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotReq != nil {
			*gotReq = *r.Clone(r.Context())
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestHTTPChecker_ReadPermissionAllows: permission ∈ {read,...} → allow.
func TestHTTPChecker_ReadPermissionAllows(t *testing.T) {
	for _, perm := range []string{"read", "triage", "write", "maintain", "admin"} {
		t.Run(perm, func(t *testing.T) {
			ts := newStubGitHub(t, http.StatusOK, fmt.Sprintf(`{"permission":%q,"user":{"login":"alice"}}`, perm), nil)
			ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
			allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "octo", "widgets")
			if err != nil || !allowed {
				t.Fatalf("perm=%q allowed=%v err=%v want true/nil", perm, allowed, err)
			}
		})
	}
}

// TestHTTPChecker_NonePermissionDenies: explicit "none" → deny without error.
func TestHTTPChecker_NonePermissionDenies(t *testing.T) {
	ts := newStubGitHub(t, http.StatusOK, `{"permission":"none","user":{"login":"alice"}}`, nil)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "octo", "widgets")
	if err != nil || allowed {
		t.Fatalf("none: allowed=%v err=%v want false/nil", allowed, err)
	}
}

// TestHTTPChecker_404Denies: GitHub 404 (user not a collaborator) → deny.
func TestHTTPChecker_404Denies(t *testing.T) {
	ts := newStubGitHub(t, http.StatusNotFound, "", nil)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "octo", "widgets")
	if err != nil || allowed {
		t.Fatalf("404: allowed=%v err=%v want false/nil", allowed, err)
	}
}

// TestHTTPChecker_403IsConfigError: GitHub 403 → ErrConfigError.
func TestHTTPChecker_403IsConfigError(t *testing.T) {
	ts := newStubGitHub(t, http.StatusForbidden, "", nil)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "octo", "widgets")
	if allowed || err == nil || !errors.Is(err, ErrConfigError) {
		t.Fatalf("403: allowed=%v err=%v want false/ErrConfigError", allowed, err)
	}
}

// TestHTTPChecker_5xxIsUnavailable: GitHub 500/502/503 → ErrUnavailable.
func TestHTTPChecker_5xxIsUnavailable(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			ts := newStubGitHub(t, status, "", nil)
			ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
			allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "octo", "widgets")
			if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("%d: allowed=%v err=%v want false/ErrUnavailable", status, allowed, err)
			}
		})
	}
}

// TestHTTPChecker_UnexpectedStatusIsUnavailable: anything else → ErrUnavailable
// rather than silently allowing.
func TestHTTPChecker_UnexpectedStatusIsUnavailable(t *testing.T) {
	ts := newStubGitHub(t, http.StatusTeapot, "", nil)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "octo", "widgets")
	if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("418: allowed=%v err=%v want false/ErrUnavailable", allowed, err)
	}
}

// TestHTTPChecker_BuildsRequest: verifies path, auth, accept, and API version
// headers — these are part of GitHub's documented contract.
func TestHTTPChecker_BuildsRequest(t *testing.T) {
	var got http.Request
	ts := newStubGitHub(t, http.StatusOK, `{"permission":"read"}`, &got)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
	if _, err := ch.HasReadAccess(context.Background(), "139674548", "alice", "octo", "widgets"); err != nil {
		t.Fatalf("HasReadAccess: %v", err)
	}
	wantPath := "/repos/octo/widgets/collaborators/alice/permission"
	if got.URL.Path != wantPath {
		t.Errorf("path = %q, want %q", got.URL.Path, wantPath)
	}
	if got.Header.Get("Authorization") != "token install-tok" {
		t.Errorf("Authorization = %q, want %q", got.Header.Get("Authorization"), "token install-tok")
	}
	if got.Header.Get("Accept") != "application/vnd.github+json" {
		t.Errorf("Accept = %q", got.Header.Get("Accept"))
	}
	if got.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", got.Header.Get("X-GitHub-Api-Version"))
	}
}

// TestHTTPChecker_URLEscapesPathParams: a userLogin or repo name with funky
// characters must be path-escaped, never spliced raw into the URL.
func TestHTTPChecker_URLEscapesPathParams(t *testing.T) {
	var got http.Request
	ts := newStubGitHub(t, http.StatusOK, `{"permission":"read"}`, &got)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("install-tok"), nil)
	if _, err := ch.HasReadAccess(context.Background(), "1", "user with space", "o", "r"); err != nil {
		t.Fatalf("HasReadAccess: %v", err)
	}
	if !strings.Contains(got.URL.EscapedPath(), "/collaborators/user%20with%20space/permission") {
		t.Errorf("path = %q does not contain escaped userLogin", got.URL.EscapedPath())
	}
}

// TestHTTPChecker_InstallTokenFailureIsUnavailable: an install-token error
// must surface as ErrUnavailable (not a silent skip / silent allow).
func TestHTTPChecker_InstallTokenFailureIsUnavailable(t *testing.T) {
	ch := NewHTTPChecker("http://nowhere.invalid", failingInstallToken(fmt.Errorf("rate limit")), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "o", "r")
	if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("install-tok-fail: allowed=%v err=%v want false/ErrUnavailable", allowed, err)
	}
}

// TestHTTPChecker_EmptyInstallTokenIsUnavailable: an empty token would create
// an "Authorization: token " header — never request, never silently allow.
func TestHTTPChecker_EmptyInstallTokenIsUnavailable(t *testing.T) {
	ch := NewHTTPChecker("http://nowhere.invalid", staticInstallToken(""), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "o", "r")
	if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty-tok: allowed=%v err=%v want false/ErrUnavailable", allowed, err)
	}
}

// TestHTTPChecker_NoInstallTokenSourceIsUnavailable: defensive — a Checker
// built with a nil InstallTokenSource fails closed.
func TestHTTPChecker_NoInstallTokenSourceIsUnavailable(t *testing.T) {
	ch := NewHTTPChecker("http://nowhere.invalid", nil, nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "o", "r")
	if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil-tok-src: allowed=%v err=%v want false/ErrUnavailable", allowed, err)
	}
}

// TestHTTPChecker_NetworkErrorIsUnavailable: TCP-level failure → ErrUnavailable.
func TestHTTPChecker_NetworkErrorIsUnavailable(t *testing.T) {
	// http://127.0.0.1:1 is reliably unreachable.
	ch := NewHTTPChecker("http://127.0.0.1:1", staticInstallToken("tok"), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "o", "r")
	if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("net-err: allowed=%v err=%v want false/ErrUnavailable", allowed, err)
	}
}

// TestHTTPChecker_MalformedJSONIsUnavailable: GitHub returned 200 but with a
// body we can't decode. Treat as unavailable rather than silently allow.
func TestHTTPChecker_MalformedJSONIsUnavailable(t *testing.T) {
	ts := newStubGitHub(t, http.StatusOK, "not-json", nil)
	ch := NewHTTPChecker(ts.URL, staticInstallToken("tok"), nil)
	allowed, err := ch.HasReadAccess(context.Background(), "1", "alice", "o", "r")
	if allowed || err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bad-json: allowed=%v err=%v want false/ErrUnavailable", allowed, err)
	}
}

// TestHTTPChecker_EmptyApiBaseUsesGitHub: an empty apiBase defaults to the
// real GitHub host. We cannot actually hit GitHub from a unit test, but we
// can verify the constructor did not panic and the type implements Checker.
func TestHTTPChecker_EmptyApiBaseUsesGitHub(t *testing.T) {
	ch := NewHTTPChecker("", staticInstallToken("tok"), nil)
	if ch == nil {
		t.Fatal("NewHTTPChecker returned nil")
	}
}
