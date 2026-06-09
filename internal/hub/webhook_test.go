package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
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
