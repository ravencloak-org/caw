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
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/store"
)

// tokenBytes is the number of cryptographically random bytes in a raw token.
const (
	tokenBytes = 32
	idBytes    = 16 // ULID-shaped: 16 bytes → 26 base32 chars
)

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

// Verifier resolves a token hash to the full Token row it was minted for. It
// is the only dependency Required has on persistence. The implementation lives
// in internal/store; *store.Store satisfies this interface structurally.
//
// The signature widened in Auth v2 Phase 1 (returns the row, not just the
// installation id) so downstream middleware can read github_user_id and
// github_user_login from the request context without a second lookup.
type Verifier interface {
	// VerifyToken looks up a row by token hash. ok is false when no row
	// matches, or the row is revoked / expired; err is non-nil only on
	// lookup failure.
	VerifyToken(hash string) (store.Token, bool, error)
}

// Context keys under which Required stores authenticated request metadata for
// downstream handlers (gin.Context.Set / .Get).
//
// ContextGitHubUserID is 0 for legacy tokens (rows that pre-date Auth v2 or
// were minted by the install_callback / installation.created webhook paths).
// Phase 2's RequireRepoAccess uses 0 as the "skip user check + emit
// Deprecation: legacy-token header" sentinel.
const (
	ContextInstallationID  = "installation_id"
	ContextGitHubUserID    = "github_user_id"
	ContextGitHubUserLogin = "github_user_login"
	ContextTokenID         = "token_id"
)

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

		tok, ok, err := v.VerifyToken(HashToken(raw))
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if !ok {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		c.Set(ContextInstallationID, tok.InstallationID)
		c.Set(ContextTokenID, tok.ID)
		// 0 = legacy token (no user binding). Downstream RequireRepoAccess
		// (Phase 2) reads this sentinel and bypasses the per-user check.
		var userID int64
		if tok.GitHubUserID != nil {
			userID = *tok.GitHubUserID
		}
		c.Set(ContextGitHubUserID, userID)
		c.Set(ContextGitHubUserLogin, tok.GitHubUserLogin)
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

// GenerateID mints a 26-char ULID-shaped identifier used for token row ids
// and auth_sessions ids. Format: 48 bits of milliseconds since the Unix epoch
// followed by 80 bits of cryptographic randomness, base32-encoded (no
// padding). Length matches the CHAR(26) column in schema.sql.
//
// We use stdlib encoding/base32 with the standard alphabet rather than
// Crockford's because Phase 1's only requirement is "26 chars, unique,
// time-ordered for index locality"; sortability on the time prefix is
// preserved either way and the standard alphabet keeps the helper dependency-
// free.
func GenerateID() (string, error) {
	var b [idBytes]byte
	// 48-bit milliseconds since epoch in the first six bytes (big-endian),
	// shifted into a uint64 for binary.PutUint64 then sliced down.
	ts := uint64(time.Now().UnixMilli())
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], ts<<16)
	copy(b[0:6], tsBuf[0:6])
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}
