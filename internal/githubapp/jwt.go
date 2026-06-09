// Package githubapp handles GitHub App authentication: JWT generation (RS256),
// installation access-token acquisition with caching, and the App Manifest flow.
package githubapp

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppJWTSigner generates GitHub App JWTs signed with an RSA private key.
type AppJWTSigner struct {
	appID      string
	privateKey *rsa.PrivateKey
}

// NewAppJWTSigner parses pemBytes (a PEM-encoded PKCS#1 or PKCS#8 RSA private
// key) and returns a signer bound to appID.
func NewAppJWTSigner(appID string, pemBytes []byte) (*AppJWTSigner, error) {
	if appID == "" {
		return nil, fmt.Errorf("githubapp: appID must not be empty")
	}
	key, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	return &AppJWTSigner{appID: appID, privateKey: key}, nil
}

// Sign returns a signed GitHub App JWT valid from now until exp (≤ 10 min per
// GitHub's limit). If exp is zero it defaults to now+9m (safe margin).
func (s *AppJWTSigner) Sign(now time.Time, exp time.Time) (string, error) {
	if exp.IsZero() {
		exp = now.Add(9 * time.Minute)
	}
	claims := jwt.RegisteredClaims{
		Issuer:    s.appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)), // clock-skew buffer
		ExpiresAt: jwt.NewNumericDate(exp),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("githubapp: sign jwt: %w", err)
	}
	return signed, nil
}

// parseRSAPrivateKey decodes a PEM block and parses an RSA private key in
// PKCS#1 (RSA PRIVATE KEY) or PKCS#8 (PRIVATE KEY) format.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("githubapp: no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("githubapp: parse PKCS1: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("githubapp: parse PKCS8: %w", err)
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("githubapp: PKCS8 key is not RSA")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("githubapp: unsupported PEM type %q", block.Type)
	}
}
