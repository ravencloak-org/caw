package githubapp_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/ravencloak-org/caw/internal/githubapp"
)

func generateTestKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, pemBytes
}

func TestNewAppJWTSigner_InvalidInputs(t *testing.T) {
	_, pemBytes := generateTestKey(t)

	tests := []struct {
		name    string
		appID   string
		pem     []byte
		wantErr bool
	}{
		{
			name:    "empty appID",
			appID:   "",
			pem:     pemBytes,
			wantErr: true,
		},
		{
			name:    "nil PEM",
			appID:   "123",
			pem:     nil,
			wantErr: true,
		},
		{
			name:    "invalid PEM content",
			appID:   "123",
			pem:     []byte("not a pem"),
			wantErr: true,
		},
		{
			name:    "valid",
			appID:   "42",
			pem:     pemBytes,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signer, err := githubapp.NewAppJWTSigner(tc.appID, tc.pem)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if signer == nil {
				t.Fatal("signer must not be nil")
			}
		})
	}
}

func TestAppJWTSigner_Sign(t *testing.T) {
	key, pemBytes := generateTestKey(t)
	signer, err := githubapp.NewAppJWTSigner("99", pemBytes)
	if err != nil {
		t.Fatalf("NewAppJWTSigner: %v", err)
	}

	now := time.Now().Truncate(time.Second)

	tests := []struct {
		name      string
		now       time.Time
		exp       time.Time
		wantIss   string
		maxExpGap time.Duration
	}{
		{
			name:      "default exp (zero → now+9m)",
			now:       now,
			exp:       time.Time{},
			wantIss:   "99",
			maxExpGap: 10 * time.Minute,
		},
		{
			name:      "explicit exp within 10m",
			now:       now,
			exp:       now.Add(8 * time.Minute),
			wantIss:   "99",
			maxExpGap: 10 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signed, err := signer.Sign(tc.now, tc.exp)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if signed == "" {
				t.Fatal("signed token must not be empty")
			}

			// Parse and verify using the public key.
			tok, err := jwt.ParseWithClaims(
				signed,
				&jwt.RegisteredClaims{},
				func(t *jwt.Token) (interface{}, error) {
					if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
						return nil, jwt.ErrSignatureInvalid
					}
					return &key.PublicKey, nil
				},
			)
			if err != nil {
				t.Fatalf("parse jwt: %v", err)
			}
			if !tok.Valid {
				t.Fatal("token must be valid")
			}

			claims, ok := tok.Claims.(*jwt.RegisteredClaims)
			if !ok {
				t.Fatal("wrong claims type")
			}

			// Issuer must equal appID.
			if claims.Issuer != tc.wantIss {
				t.Errorf("iss = %q; want %q", claims.Issuer, tc.wantIss)
			}

			// iat must be ≤ now (we set iat = now - 30s for clock-skew buffer).
			iat := claims.IssuedAt.Time
			if iat.After(tc.now) {
				t.Errorf("iat %v is after now %v", iat, tc.now)
			}

			// exp must be ≤ now + 10 min (GitHub's hard limit).
			expTime := claims.ExpiresAt.Time
			ceiling := tc.now.Add(tc.maxExpGap)
			if expTime.After(ceiling) {
				t.Errorf("exp %v exceeds ceiling %v", expTime, ceiling)
			}
			// exp must be after now.
			if !expTime.After(tc.now) {
				t.Errorf("exp %v must be after now %v", expTime, tc.now)
			}

			// Algorithm must be RS256.
			if tok.Method.Alg() != "RS256" {
				t.Errorf("alg = %q; want RS256", tok.Method.Alg())
			}
		})
	}
}
