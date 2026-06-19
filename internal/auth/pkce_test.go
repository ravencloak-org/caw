package auth

import (
	"strings"
	"testing"
)

// rfc7636Vector is the canonical S256 example from RFC 7636 Appendix B. If
// this test ever fails, our PKCE implementation has drifted from the spec
// and every other test in this file is suspect.
func TestS256Challenge_RFC7636Vector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const wantChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	got := S256Challenge(verifier)
	if got != wantChallenge {
		t.Errorf("S256Challenge = %q, want %q", got, wantChallenge)
	}
}

func TestGeneratePKCE_RoundTripsViaVerify(t *testing.T) {
	for i := 0; i < 16; i++ {
		v, ch, err := GeneratePKCE()
		if err != nil {
			t.Fatalf("GeneratePKCE: %v", err)
		}
		if got := len(v); got != 43 {
			t.Errorf("verifier len = %d, want 43", got)
		}
		if err := ValidateVerifier(v); err != nil {
			t.Errorf("ValidateVerifier(%q): %v", v, err)
		}
		if err := VerifyPKCE(v, ch, "S256"); err != nil {
			t.Errorf("VerifyPKCE(round-trip): %v", err)
		}
		// Sanity: the challenge must NEVER equal the verifier (would mean
		// we forgot to hash).
		if v == ch {
			t.Errorf("verifier and challenge are equal: %q", v)
		}
	}
}

func TestVerifyPKCE_RejectsTampering(t *testing.T) {
	v, ch, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}

	// Mutate one verifier byte → mismatch.
	mutated := []byte(v)
	mutated[0] ^= 0x01
	if !validVerifierChar(mutated[0]) {
		// If the flip landed on a non-alphabet byte, bump it to one we know
		// is in the alphabet so we test "mismatch" not "invalid char".
		mutated[0] = 'A'
	}
	if err := VerifyPKCE(string(mutated), ch, "S256"); err == nil {
		t.Errorf("expected mismatch error for mutated verifier")
	}

	// Verify against a different challenge → mismatch.
	if err := VerifyPKCE(v, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "S256"); err == nil {
		t.Errorf("expected mismatch error for wrong challenge")
	}
}

func TestVerifyPKCE_RejectsUnsupportedMethod(t *testing.T) {
	v, ch, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	for _, method := range []string{"plain", "PLAIN", "S384", ""} {
		if err := VerifyPKCE(v, ch, method); err == nil {
			t.Errorf("method=%q: expected error", method)
		}
	}
	// Case-insensitive accept of "S256".
	if err := VerifyPKCE(v, ch, "s256"); err != nil {
		t.Errorf("method=s256: expected accept, got %v", err)
	}
}

func TestValidateVerifier_LengthBounds(t *testing.T) {
	short := strings.Repeat("a", 42)
	if err := ValidateVerifier(short); err == nil {
		t.Errorf("42-char verifier accepted, want length error")
	}
	just := strings.Repeat("a", 43)
	if err := ValidateVerifier(just); err != nil {
		t.Errorf("43-char verifier rejected: %v", err)
	}
	maxv := strings.Repeat("a", 128)
	if err := ValidateVerifier(maxv); err != nil {
		t.Errorf("128-char verifier rejected: %v", err)
	}
	tooLong := strings.Repeat("a", 129)
	if err := ValidateVerifier(tooLong); err == nil {
		t.Errorf("129-char verifier accepted, want length error")
	}
}

func TestValidateVerifier_Alphabet(t *testing.T) {
	// Every legal char individually padded to length 43.
	allowed := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	for _, r := range allowed {
		v := string(r) + strings.Repeat("a", 42)
		if err := ValidateVerifier(v); err != nil {
			t.Errorf("char %q rejected: %v", r, err)
		}
	}
	for _, r := range "+/= !@#$%^&*()" {
		v := string(r) + strings.Repeat("a", 42)
		if err := ValidateVerifier(v); err == nil {
			t.Errorf("char %q accepted but should fail", r)
		}
	}
}
