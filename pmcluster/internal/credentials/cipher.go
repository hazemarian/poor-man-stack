// Package credentials handles at-rest encryption for sensitive values that
// pmcluster stores in SQLite (managed component passwords, registry creds,
// webhook secrets).
//
// The encryption key is generated on first use and persisted to a file at
// $DataDir/.encryption_key (mode 0600, owner-only). Loss of that file
// renders all stored credentials unrecoverable — operators are responsible
// for backing it up alongside the SQLite DB.
//
// Algorithm: AES-256-GCM. Nonce is 12 random bytes per ciphertext, prepended
// to the output. Tampered ciphertext fails decryption with a clean error.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// keyBytes is the AES-256 key length.
const keyBytes = 32

// nonceBytes is the GCM nonce length.
const nonceBytes = 12

// ErrInvalidCiphertext indicates the ciphertext is malformed (too short to
// contain a nonce) or has been tampered with (GCM auth tag mismatch).
var ErrInvalidCiphertext = errors.New("invalid or tampered ciphertext")

// Cipher encrypts and decrypts byte slices with a key loaded from disk.
// Construct via Open. Safe for concurrent use after construction.
type Cipher struct {
	gcm cipher.AEAD
}

// Open loads the encryption key from path, creating it (mode 0600) on first
// use. Returns a Cipher ready for Encrypt/Decrypt calls.
//
// Parent directory must already exist with safe permissions; pmcluster init
// creates ~/.pmcluster/ with mode 0700.
func Open(path string) (*Cipher, error) {
	key, err := loadOrCreateKey(path)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	return &Cipher{gcm: gcm}, nil
}

// Encrypt produces ciphertext = nonce(12B) || gcm_seal(plaintext).
// Each call generates a fresh nonce.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	sealed := c.gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Decrypt reverses Encrypt. Returns ErrInvalidCiphertext for any tamper or
// malformed input — never panics, never leaks plaintext on auth failure.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < nonceBytes {
		return nil, ErrInvalidCiphertext
	}
	nonce, sealed := ciphertext[:nonceBytes], ciphertext[nonceBytes:]
	plain, err := c.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plain, nil
}

// loadOrCreateKey reads the AES key from disk; if missing, generates a fresh
// 32-byte key and writes it with mode 0600. Validates length when reading.
func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != keyBytes {
			return nil, fmt.Errorf("key file %s has wrong length (%d, expected %d)", path, len(data), keyBytes)
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file: %w", err)
	}

	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	// O_EXCL: never overwrite an existing file (race-safe vs another pmcluster
	// process initialising at the same time — though that's vanishingly rare
	// since `pmcluster init` is the one place this gets called concurrently).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create key file: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close key file: %w", err)
	}
	return key, nil
}
