// Package auth provides secret generation, hashing and verification for
// agent API keys and admin keys. Agent keys are stored as bcrypt hashes.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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
