// Package auth implements installation-token authentication for the Caw Hub's
// authenticated endpoints (SSE subscribe, get_pending, ack).
//
// A Watcher presents a Hub-minted token bound to a GitHub App installation
// (see docs/adr/0003-sse-auth-via-hub-minted-installation-token.md). The raw
// token never leaves the client; the Hub stores only its SHA-256 hash and
// resolves the owning installation by hash on each request.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// tokenBytes is the number of cryptographically random bytes in a raw token.
const tokenBytes = 32

// GenerateToken mints a new installation token. It returns the raw token to
// hand to the client (base64 URL encoding, no padding) and the lowercase hex
// SHA-256 hash to persist server-side. The raw token is never stored.
func GenerateToken() (raw string, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	hash = HashToken(raw)
	return raw, hash, nil
}

// HashToken returns the lowercase hex SHA-256 hash of a raw token. It is the
// canonical way to derive the stored credential from a presented token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Verifier resolves a token hash to the installation it was minted for. It is
// the only dependency Required has on persistence, keeping this package free of
// any store import.
type Verifier interface {
	// VerifyToken looks up an installation by token hash. ok is false when no
	// installation matches the hash; err is non-nil only on lookup failure.
	VerifyToken(hash string) (installationID string, ok bool, err error)
}

// ContextInstallationID is the gin context key under which Required stores the
// authenticated installation ID for downstream handlers.
const ContextInstallationID = "installation_id"

// Required returns gin middleware that authenticates the request against v.
//
// It reads the bearer token from "Authorization: Bearer <token>", falling back
// to the "X-Caw-Token: <token>" header. A missing or unknown token yields 401;
// a Verifier failure yields 500. On success it sets the installation ID in the
// context and proceeds to the next handler.
func Required(v Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractToken(c)
		if raw == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		installationID, ok, err := v.VerifyToken(HashToken(raw))
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if !ok {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		c.Set(ContextInstallationID, installationID)
		c.Next()
	}
}

// extractToken pulls the raw token from the Authorization bearer header,
// falling back to X-Caw-Token. It returns "" when neither supplies one.
func extractToken(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		if token, found := strings.CutPrefix(h, "Bearer "); found {
			if token = strings.TrimSpace(token); token != "" {
				return token
			}
		}
	}
	return strings.TrimSpace(c.GetHeader("X-Caw-Token"))
}
