// Package auth handles pmcluster bearer tokens: generation, argon2id
// hashing, and constant-time comparison.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const rawTokenBytes = 32 // 256 bits → ~43 base64url chars

// argon2id parameters per OWASP 2024 guidance for interactive auth.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // KiB → 64 MiB
	argonThreads uint8  = 1
	argonSaltLen uint32 = 16
	argonKeyLen  uint32 = 32
)

// hashPrefix tags the format so future algorithm changes are distinguishable.
const hashPrefix = "argon2id$v1$"

// ErrInvalidToken: malformed plaintext (distinct from "valid format, wrong value").
var ErrInvalidToken = errors.New("invalid token format")

// ErrInvalidHash: stored hash blob is corrupt — DB tampering or version skew.
var ErrInvalidHash = errors.New("invalid stored hash")

func GenerateToken() (string, error) {
	buf := make([]byte, rawTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken hashes with argon2id; the returned string encodes the salt
// so VerifyToken needs no additional state.
func HashToken(token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	digest := argon2.IDKey([]byte(token), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return hashPrefix + hex.EncodeToString(salt) + "$" + hex.EncodeToString(digest), nil
}

// VerifyToken returns true iff token hashes to stored. Constant-time.
func VerifyToken(token, stored string) (bool, error) {
	if !strings.HasPrefix(stored, hashPrefix) {
		return false, ErrInvalidHash
	}
	parts := strings.Split(strings.TrimPrefix(stored, hashPrefix), "$")
	if len(parts) != 2 {
		return false, ErrInvalidHash
	}
	salt, err := hex.DecodeString(parts[0])
	if err != nil || len(salt) != int(argonSaltLen) {
		return false, ErrInvalidHash
	}
	want, err := hex.DecodeString(parts[1])
	if err != nil || len(want) != int(argonKeyLen) {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(token), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(want, got) == 1, nil
}
