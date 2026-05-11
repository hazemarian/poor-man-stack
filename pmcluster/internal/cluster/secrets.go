package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"golang.org/x/crypto/bcrypt"
)

// EnsureSecret creates a Swarm secret if one with this name doesn't
// exist; returns true iff newly created. Existing secrets are NEVER
// modified — rotation is "remove + recreate + force-redeploy", handled
// by CredentialsManager.Rotate.
//
// pmcluster-managed secrets carry the pmclusterLabel so `cluster down
// --purge` can target them without touching operator-created secrets.
func EnsureSecret(ctx context.Context, d docker.Client, name string, data []byte) (created bool, err error) {
	exists, err := d.SecretExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check secret %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = d.SecretCreate(ctx, docker.SecretSpec{
		Name: name,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel: "true",
		},
	})
	if err != nil {
		return false, fmt.Errorf("create secret %s: %w", name, err)
	}
	return true, nil
}

func EnsureSecretFromFile(ctx context.Context, d docker.Client, name, path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureSecret(ctx, d, name, data)
}

// EnsureConfig is the Docker-config analogue of EnsureSecret. Configs are
// also immutable; same rotation dance applies.
func EnsureConfig(ctx context.Context, d docker.Client, name string, data []byte) (created bool, err error) {
	exists, err := d.ConfigExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check config %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = d.ConfigCreate(ctx, docker.ConfigSpec{
		Name: name,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel: "true",
		},
	})
	if err != nil {
		return false, fmt.Errorf("create config %s: %w", name, err)
	}
	return true, nil
}

// RandomPassword returns a 24-byte (~32-char base64url) random password.
func RandomPassword() (string, error) {
	const entropy = 24
	buf := make([]byte, entropy)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HtpasswdLine produces "user:bcrypt-hash\n" for Traefik's basicAuth
// middleware. Cost 10 ≈ 100 ms/hash — defensive without slowing startup.
func HtpasswdLine(user, password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return fmt.Sprintf("%s:%s\n", user, hash), nil
}
