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

// EnsureSecret creates a Docker Swarm secret from `data` if a secret with
// that name doesn't already exist. Returns true iff newly created.
//
// CRITICAL: Existing secrets are NEVER modified. To rotate, callers must
// explicitly remove the secret first AND force-redeploy any consuming
// services (Phase 2.2 `pmcluster credentials rotate` does this dance).
//
// All pmcluster-managed secrets get a label so `cluster down --purge` can
// identify them without nuking operator-created secrets.
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

// EnsureSecretFromFile is a thin convenience wrapper around EnsureSecret
// for cert/key/etc. that come from disk.
func EnsureSecretFromFile(ctx context.Context, d docker.Client, name, path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureSecret(ctx, d, name, data)
}

// EnsureConfig is the Docker-config analogue of EnsureSecret. Used for
// rendered YAML payloads (OTel pipeline, Traefik dynamic) that bundled
// services need at known paths. Replicated to every node by Swarm.
//
// Like secrets, configs are immutable; rotation is "remove + recreate +
// force-redeploy consuming services" (handled by `pmcluster cluster
// reconcile` in a later phase, or `cluster down --purge` + `cluster up`).
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

// RandomPassword returns a cryptographically random URL-safe password of
// approximately 32 characters (24 bytes of entropy, base64url-encoded).
//
// Used for bootstrap credentials (Traefik admin, Portainer admin,
// OpenObserve admin). NOT used for the pmcluster admin token — that uses
// auth.GenerateToken (also random, but separately scoped).
func RandomPassword() (string, error) {
	const entropy = 24
	buf := make([]byte, entropy)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HtpasswdLine produces an Apache htpasswd entry "user:bcrypt-hash\n"
// suitable for Traefik's basicAuth middleware (which reads `usersFile`).
//
// Cost 10 = ~100 ms per hash on modern hardware: a sweet spot between
// "fast enough that startup isn't perceptibly slow" and "expensive enough
// that the hash has some defensive value if the secret leaks".
func HtpasswdLine(user, password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return fmt.Sprintf("%s:%s\n", user, hash), nil
}
