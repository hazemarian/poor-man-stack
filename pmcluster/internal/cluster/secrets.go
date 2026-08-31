package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

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

// EnsureVersionedSecretFromFile reads a file and creates a versioned Swarm
// secret (e.g. cert_v001, cert_v002). Returns the versioned name.
func EnsureVersionedSecretFromFile(ctx context.Context, d docker.Client, baseName, path string) (versionedName string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return EnsureVersionedSecret(ctx, d, baseName, data)
}

// EnsureVersionedSecret creates a versioned Swarm secret (e.g. cert_v001)
// and garbage-collects old versions. Same pattern as EnsureConfig.
func EnsureVersionedSecret(ctx context.Context, d docker.Client, baseName string, data []byte) (versionedName string, err error) {
	existing, err := d.SecretList(ctx, pmclusterLabel, "true")
	if err != nil {
		return "", fmt.Errorf("list secrets: %w", err)
	}

	prefix := baseName + "_v"
	maxVer := 0
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		var v int
		if _, scanErr := fmt.Sscanf(name, prefix+"%d", &v); scanErr == nil && v > maxVer {
			maxVer = v
		}
	}

	newVer := maxVer + 1
	versionedName = fmt.Sprintf("%s_v%03d", baseName, newVer)

	err = d.SecretCreate(ctx, docker.SecretSpec{
		Name: versionedName,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel:   "true",
			"pmcluster.base": baseName,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create secret %s: %w", versionedName, err)
	}

	// GC old versions.
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == versionedName {
			continue
		}
		if rmErr := d.SecretRemove(ctx, name); rmErr != nil {
			_ = rmErr // old secret may still be in use; GC next time
		}
	}

	return versionedName, nil
}

// EnsureConfig is the Docker-config analogue of EnsureSecret. Because Docker
// configs are immutable, we create a versioned name (e.g.
// pmcluster_otel_config_v001) and let the caller embed the versioned name
// into the compose file via __CONFIG_NAME__ placeholder. Old versions are
// cleaned up after the new one is created. Returns the full versioned name
// actually created so callers can substitute it into templates.
//
// baseName is the logical name ("pmcluster_otel_config"). The versioned name
// is baseName + "_v" + zero-padded sequence.
func EnsureConfig(ctx context.Context, d docker.Client, baseName string, data []byte, version string) (versionedName string, err error) {
	// List existing versioned configs with the pmcluster label and baseName
	// prefix so we can find the next version number.
	existing, err := d.ConfigList(ctx, pmclusterLabel, "true")
	if err != nil {
		return "", fmt.Errorf("list configs: %w", err)
	}

	prefix := baseName + "_v"
	maxVer := 0
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		var v int
		if _, scanErr := fmt.Sscanf(name, prefix+"%d", &v); scanErr == nil && v > maxVer {
			maxVer = v
		}
	}

	newVer := maxVer + 1
	versionedName = fmt.Sprintf("%s_v%03d", baseName, newVer)

	err = d.ConfigCreate(ctx, docker.ConfigSpec{
		Name: versionedName,
		Data: data,
		Labels: map[string]string{
			pmclusterLabel:      "true",
			"pmcluster.base":    baseName,
			"pmcluster.version": version,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create config %s: %w", versionedName, err)
	}

	// Garbage-collect old versions (keep only the one we just created).
	for _, name := range existing {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if name == versionedName {
			continue
		}
		if rmErr := d.ConfigRemove(ctx, name); rmErr != nil {
			// Old config may still be in use; that's fine — it'll be
			// cleaned up on the next cluster up after stacks redeploy.
			// Don't fail the operation.
			_ = rmErr
		}
	}

	return versionedName, nil
}

// RandomPassword returns a cryptographically random password that meets
// OpenObserve complexity requirements: 8-128 characters with at least one
// lowercase letter, one uppercase letter, one digit, and one special character.
func RandomPassword() (string, error) {
	const (
		entropy      = 24
		special      = "!@#$%^&*"
		lowerLetters = "abcdefghijklmnopqrstuvwxyz"
		upperLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digits       = "0123456789"
	)

	buf := make([]byte, entropy)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	// Base64-encode the random bytes, then append one guaranteed
	// character of each required type.  The result is ~38 chars and
	// always satisfies the OpenObserve password policy.
	base := base64.RawURLEncoding.EncodeToString(buf)

	// Pick one guaranteed char from each required set using the
	// first few entropy bytes (we already consumed buf, so use the
	// raw bytes directly before encoding for the indices).
	guaranteed := []byte{
		lowerLetters[int(buf[0])%len(lowerLetters)],
		upperLetters[int(buf[1])%len(upperLetters)],
		digits[int(buf[2])%len(digits)],
		special[int(buf[3])%len(special)],
	}

	// Shuffle the guaranteed chars into random positions in the base
	// string to avoid predictable placement.
	b := []byte(base)
	for i, gc := range guaranteed {
		pos := int(buf[4+i]) % len(b)
		b = append(b, 0)
		copy(b[pos+1:], b[pos:])
		b[pos] = gc
	}

	return string(b), nil
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
