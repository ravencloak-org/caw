package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ravencloak-org/caw/internal/github"
)

func sign(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("s3cr3t")
	payload := []byte(`{"hello":"world"}`)
	good := sign(secret, payload)

	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		want   bool
	}{
		{"valid", secret, payload, good, true},
		{"wrong secret", []byte("nope"), payload, good, false},
		{"tampered body", secret, []byte(`{"hello":"evil"}`), good, false},
		{"missing prefix", secret, payload, hex.EncodeToString([]byte("x")), false},
		{"empty header", secret, payload, "", false},
		{"not hex", secret, payload, "sha256=zzzz", false},
		{"wrong length", secret, payload, "sha256=ab", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifySignature(tt.secret, tt.body, tt.header); got != tt.want {
				t.Fatalf("VerifySignature(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestDeriveRound(t *testing.T) {
	mk := func() github.Envelope {
		var e github.Envelope
		e.Repository.Owner.Login = "ravencloak-org"
		e.Repository.Name = "caw"
		return e
	}

	t.Run("from pull_request", func(t *testing.T) {
		e := mk()
		e.PullRequest = &struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}{Number: 42}
		e.PullRequest.Head.SHA = "deadbeef"
		key, ok := DeriveRound(e)
		if !ok || key.String() != "ravencloak-org/caw#42@deadbeef" {
			t.Fatalf("got %v ok=%v", key, ok)
		}
	})

	t.Run("from check_suite", func(t *testing.T) {
		e := mk()
		e.CheckSuite = &struct {
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		}{HeadSHA: "cafef00d", PullRequests: []struct {
			Number int `json:"number"`
		}{{Number: 7}}}
		key, ok := DeriveRound(e)
		if !ok || key.String() != "ravencloak-org/caw#7@cafef00d" {
			t.Fatalf("got %v ok=%v", key, ok)
		}
	})

	t.Run("no PR context", func(t *testing.T) {
		if _, ok := DeriveRound(mk()); ok {
			t.Fatal("expected ok=false when no PR/check_suite present")
		}
	})

	t.Run("no repository", func(t *testing.T) {
		if _, ok := DeriveRound(github.Envelope{}); ok {
			t.Fatal("expected ok=false when repository is absent")
		}
	})
}
