// Package auth — PKCE (Proof Key for Code Exchange, RFC 7636) helpers for the
// Auth v2 MCP login handoff (Phase 3+).
//
// Why PKCE here: the MCP plugin opens a browser to /auth/u/:session_id, the
// browser eventually returns the OAuth result to either a loopback HTTP
// listener (default) or via device-flow polling. PKCE binds those two ends so
// a rogue process that intercepts the loopback port (or the device code)
// cannot redeem the resulting TokenBundle without also holding the verifier
// the MCP keeps in memory.
//
// We implement S256 only (RFC 7636 §4.2): no plain, no SHA-1.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	// PKCEMethod is the only code_challenge_method this package supports.
	// RFC 7636 §4.3.
	PKCEMethod = "S256"

	// pkceVerifierBytes is the entropy of a freshly minted verifier (32 bytes
	// → 43 base64url chars). RFC 7636 §4.1 requires 43-128 chars when
	// base64url-encoded; 43 sits at the minimum and gives 256 bits of entropy.
	pkceVerifierBytes = 32

	// pkceVerifierMinLen / pkceVerifierMaxLen bracket the legal range from
	// RFC 7636 §4.1. Verifiers outside the range are rejected by Verify.
	pkceVerifierMinLen = 43
	pkceVerifierMaxLen = 128
)

// pkceVerifierAlphabet is the set of characters RFC 7636 §4.1 permits in a
// code_verifier: ALPHA / DIGIT / "-" / "." / "_" / "~". The check below uses
// inline byte arithmetic rather than a regexp so it stays branch-cheap on the
// hot verify path.
func validVerifierChar(b byte) bool {
	switch {
	case 'A' <= b && b <= 'Z':
		return true
	case 'a' <= b && b <= 'z':
		return true
	case '0' <= b && b <= '9':
		return true
	case b == '-' || b == '.' || b == '_' || b == '~':
		return true
	}
	return false
}

// GeneratePKCE mints a fresh (verifier, challenge) pair. The verifier is 43
// chars of base64url-encoded 32-byte random; the challenge is the S256
// transform of that verifier, both as RFC 7636 §4 describes.
//
// Callers MUST keep the verifier private — leaking it nullifies the binding.
func GeneratePKCE() (verifier, challenge string, err error) {
	b := make([]byte, pkceVerifierBytes)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("pkce read random: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	challenge = S256Challenge(verifier)
	return verifier, challenge, nil
}

// S256Challenge returns the base64url-encoded SHA-256 of verifier with no
// padding. This is the value the MCP sends to /auth/start as code_challenge
// and the hub later compares against sha256(verifier) on the device-flow
// /auth/poll path.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidateVerifier reports whether s is a structurally legal code_verifier
// per RFC 7636 §4.1 (43-128 chars, unreserved alphabet). It does NOT compare
// against any challenge — that is VerifyPKCE's job.
func ValidateVerifier(s string) error {
	if n := len(s); n < pkceVerifierMinLen || n > pkceVerifierMaxLen {
		return fmt.Errorf("pkce: verifier length %d outside [%d, %d]",
			n, pkceVerifierMinLen, pkceVerifierMaxLen)
	}
	for i := 0; i < len(s); i++ {
		if !validVerifierChar(s[i]) {
			return fmt.Errorf("pkce: verifier char %q at offset %d is not in the RFC 7636 unreserved alphabet",
				s[i], i)
		}
	}
	return nil
}

// VerifyPKCE confirms that the S256 hash of verifier equals challenge using a
// constant-time comparison. method must be "S256" (we do not support "plain").
// The challenge is whatever was stored on the auth_sessions row; the verifier
// is presented at exchange time by the MCP.
//
// A length mismatch, a non-S256 method, or a verifier that fails
// ValidateVerifier all return an error rather than a bare false so callers can
// log the precise failure reason on the audit path.
func VerifyPKCE(verifier, challenge, method string) error {
	if !strings.EqualFold(method, PKCEMethod) {
		return fmt.Errorf("pkce: unsupported method %q (only %q is supported)",
			method, PKCEMethod)
	}
	if err := ValidateVerifier(verifier); err != nil {
		return err
	}
	got := S256Challenge(verifier)
	if subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) != 1 {
		return fmt.Errorf("pkce: code_verifier does not match stored challenge")
	}
	return nil
}
