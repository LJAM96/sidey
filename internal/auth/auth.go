// Package auth provides secret generation, hashing and verification for
// agent API keys and admin keys. Agent keys are stored as bcrypt hashes.
package auth

import "strings"

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 10

// GenerateSecret returns a fresh 32 byte random secret in URL safe base64.
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// KeyID returns a fixed length identifier for a secret, derived from its
// sha256. It is safe to store alongside the bcrypt hash: knowing the id does
// not reveal the secret, and it enables indexed lookup by the raw key.
func KeyID(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// HashSecret hashes a secret for storage.
func HashSecret(secret string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash secret: %w", err)
	}
	return string(hash), nil
}

// VerifySecret reports whether secret matches the stored bcrypt hash.
func VerifySecret(secret, storedHash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(secret)) == nil
}

// ConstantTimeEqual compares two strings in constant time.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// FormatEnrolmentToken packages a secret with its public key id so that an
// unauthenticated enrolment request can resolve exactly one candidate row
// before running the single bcrypt verification, rather than scanning every
// outstanding token hash (a bcrypt CPU amplification vector).
func FormatEnrolmentToken(secret string) string {
	return KeyID(secret) + "." + secret
}

// ParseEnrolmentToken splits a token of the form "<keyid>.<secret>" back
// into its public identifier and secret. It reports false for legacy tokens
// that do not carry a key id prefix.
func ParseEnrolmentToken(token string) (keyID, secret string, ok bool) {
	i := strings.IndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", "", false
	}
	return token[:i], token[i+1:], true
}
