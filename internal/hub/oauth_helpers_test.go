package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// exchangeOAuthCodeShared / listUserInstallations / fetchUser are the three
// helpers Phase 3 extracted from install_callback so the new auth-session
// handler can reuse them. Each has 3-4 branches that legacy tests don't
// exercise — covered here.

func TestExchangeOAuthCodeShared_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-123","token_type":"bearer","scope":""}`))
	}))
	defer srv.Close()
	tok, err := exchangeOAuthCodeShared(context.Background(), http.DefaultClient, srv.URL, "cid", "secret", "code")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if tok != "tok-123" {
		t.Errorf("token = %q, want tok-123", tok)
	}
}

func TestExchangeOAuthCodeShared_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	_, err := exchangeOAuthCodeShared(context.Background(), http.DefaultClient, srv.URL, "cid", "secret", "code")
	if err == nil || !strings.Contains(err.Error(), "oauth status 500") {
		t.Errorf("want oauth status error, got %v", err)
	}
}

func TestExchangeOAuthCodeShared_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()
	_, err := exchangeOAuthCodeShared(context.Background(), http.DefaultClient, srv.URL, "cid", "secret", "code")
	if err == nil || !strings.Contains(err.Error(), "decode oauth response") {
		t.Errorf("want decode error, got %v", err)
	}
}

func TestExchangeOAuthCodeShared_MissingAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// GitHub returns 200 with error field when the code is bad — handler
		// must surface "no access_token" rather than silently succeeding.
		_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`))
	}))
	defer srv.Close()
	_, err := exchangeOAuthCodeShared(context.Background(), http.DefaultClient, srv.URL, "cid", "secret", "code")
	if err == nil || !strings.Contains(err.Error(), "no access_token") {
		t.Errorf("want no access_token error, got %v", err)
	}
}

func TestExchangeOAuthCodeShared_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// never responds — context cancels
		<-make(chan struct{})
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	_, err := exchangeOAuthCodeShared(ctx, http.DefaultClient, srv.URL, "cid", "secret", "code")
	if err == nil {
		t.Fatal("want context-canceled error")
	}
}

func TestListUserInstallations_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/installations" {
			http.Error(w, "wrong path", 404)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token user-tok" {
			t.Errorf("Authorization = %q, want token user-tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"installations":[{"id":42,"account":{"login":"acme"}},{"id":99,"account":{"login":"beta"}}]}`))
	}))
	defer srv.Close()
	insts, err := listUserInstallations(context.Background(), http.DefaultClient, srv.URL, "user-tok")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(insts) != 2 || insts[0].ID != 42 || insts[1].ID != 99 {
		t.Errorf("unexpected installs: %+v", insts)
	}
}

func TestListUserInstallations_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limit", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := listUserInstallations(context.Background(), http.DefaultClient, srv.URL, "user-tok")
	if err == nil || !strings.Contains(err.Error(), "installations status 403") {
		t.Errorf("want installations status 403, got %v", err)
	}
}

func TestListUserInstallations_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"installations": "not-an-array"`)) // truncated
	}))
	defer srv.Close()
	_, err := listUserInstallations(context.Background(), http.DefaultClient, srv.URL, "user-tok")
	if err == nil || !strings.Contains(err.Error(), "decode installations") {
		t.Errorf("want decode error, got %v", err)
	}
}

func TestFetchUser_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.Error(w, "wrong path", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":12345,"login":"alice"}`))
	}))
	defer srv.Close()
	u, err := fetchUser(context.Background(), http.DefaultClient, srv.URL, "user-tok")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if u.ID != 12345 || u.Login != "alice" {
		t.Errorf("user = %+v, want id=12345 login=alice", u)
	}
}

func TestFetchUser_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := fetchUser(context.Background(), http.DefaultClient, srv.URL, "user-tok")
	if err == nil {
		t.Fatalf("want error on 401")
	}
}

func TestFetchUser_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()
	_, err := fetchUser(context.Background(), http.DefaultClient, srv.URL, "user-tok")
	if err == nil {
		t.Fatal("want decode error on non-JSON body")
	}
}
