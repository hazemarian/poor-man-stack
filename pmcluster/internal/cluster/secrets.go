package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
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

// EnsureConfig is the Docker-config analogue of EnsureSecret. Configs are
// also immutable; same rotation dance applies.
func EnsureConfig(ctx context.Context, d docker.Client, name string, data []byte) (created bool, err error) {
	exists, err := d.ConfigExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check config %s: %w", name, err)
	}
	if exists {
		if err := d.ConfigRemove(ctx, name); err != nil {
			// Config may be in use by a running service — detach it first.
			if strings.Contains(err.Error(), "is in use") {
				if detachErr := detachConfigFromServices(ctx, name); detachErr != nil {
					return false, fmt.Errorf("detach config %s from services: %w", name, detachErr)
				}
				if err2 := d.ConfigRemove(ctx, name); err2 != nil {
					return false, fmt.Errorf("remove existing config %s (after detach): %w", name, err2)
				}
			} else {
				return false, fmt.Errorf("remove existing config %s: %w", name, err)
			}
		}
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

// detachConfigFromServices finds all Swarm services that mount configName
// and runs `docker service update --config-rm <config> <service>` for each.
// This is only needed when ConfigRemove fails because the config is in use.
func detachConfigFromServices(ctx context.Context, configName string) error {
	out, err := exec.CommandContext(ctx, "docker", "service", "ls",
		"--format", "{{.Name}}",
		"--filter", "mode=replicated,global",
	).Output()
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	for _, svc := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		svc = strings.TrimSpace(svc)
		if svc == "" {
			continue
		}
		// Check if this service mounts the config
		inspectOut, err := exec.CommandContext(ctx, "docker", "service", "inspect",
			"--format", "{{range .Spec.TaskTemplate.ContainerSpec.Configs}}{{.ConfigName}} {{end}}",
			svc,
		).Output()
		if err != nil {
			continue // skip services we can't inspect
		}
		if strings.Contains(string(inspectOut), configName) {
			cmd := exec.CommandContext(ctx, "docker", "service", "update",
				"--config-rm", configName,
				"--force",
				"--detach=true",
				svc,
			)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("detach %s from %s: %w", configName, svc, err)
			}
		}
	}
	return nil
}
