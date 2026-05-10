// Package auth handles pmcluster bearer tokens: generation, hashing
// (argon2id), and constant-time comparison.
//
// Tokens are random 32-byte values, base64url-encoded for transport. We store
// only argon2id hashes — the plaintext token is shown to the operator exactly
// once at creation time, then discarded.
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

// rawTokenBytes is the entropy of the underlying token (256 bits). The
// base64-encoded representation users see is ~43 characters long.
const rawTokenBytes = 32

// argon2id parameters. Defaults follow OWASP 2024 guidance for interactive
// auth tokens: t=2, m=64MiB, p=1, salt=16B, key=32B.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // KiB → 64 MiB
	argonThreads uint8  = 1
	argonSaltLen uint32 = 16
	argonKeyLen  uint32 = 32
)

// hashPrefix tags the encoded hash format so future algorithm changes are
// distinguishable. v1 = argon2id with the parameters above.
const hashPrefix = "argon2id$v1$"

// ErrInvalidToken indicates the supplied token is malformed (wrong length,
// wrong encoding) — distinct from "valid format, wrong value".
var ErrInvalidToken = errors.New("invalid token format")

// ErrInvalidHash indicates the stored hash blob is malformed. Should never
// happen in practice; surfaces only on DB tampering or version skew.
var ErrInvalidHash = errors.New("invalid stored hash")

// GenerateToken returns a cryptographically random base64url-encoded token
// of ~43 characters (rawTokenBytes of entropy). Show this to the operator
// once; we only ever store its hash.
func GenerateToken() (string, error) {
	buf := make([]byte, rawTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken hashes a plaintext token with argon2id and returns an opaque
// string suitable for storage in users.token_hash. The format encodes the
// salt and parameters so verification needs no additional state.
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

// VerifyToken returns true iff token hashes to the stored value. Uses a
// constant-time comparison; safe against timing attacks.
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
