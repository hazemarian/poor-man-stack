//go:build e2e

// Package e2e contains end-to-end tests for the pmcluster binary.
// This file tests `pmcluster cluster up` against a real single-node Docker
// Swarm. It is gated behind the PMCLUSTER_E2E_SWARM=1 env var so it never
// runs by accident on developer machines — CI sets the variable explicitly.
//
// Run via: PMCLUSTER_E2E_SWARM=1 make e2e
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestClusterUp exercises `pmcluster cluster up` against a real Docker Swarm.
//
// Skip conditions:
//   - PMCLUSTER_E2E_SWARM != "1"
//   - `docker` binary not on PATH
//
// The test initialises a single-node Swarm if one is not already active, then
// drives the full cluster up → idempotent re-run → cluster down --purge flow.
func TestClusterUp(t *testing.T) {
	// ── Guard: env var ────────────────────────────────────────────────────────
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping cluster up e2e (set it in CI or locally to enable)")
	}

	// ── Guard: docker binary ──────────────────────────────────────────────────
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping cluster up e2e")
	}

	// Cap the whole test to 5 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ── Swarm bootstrap ───────────────────────────────────────────────────────
	// We check whether Swarm is already active before touching it so running
	// this test on a real manager doesn't clobber the existing Swarm state.
	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		// Only leave the Swarm we created ourselves, not one that pre-existed.
		t.Cleanup(func() {
			t.Log("TestClusterUp: leaving Swarm we initialised (cleanup)")
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	// ── pmcluster init ────────────────────────────────────────────────────────
	homeDir := t.TempDir()
	stdout, _, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", code, stdout)
	}
	t.Logf("pmcluster init OK (home=%s)", homeDir)

	// ── Self-signed TLS cert + key ────────────────────────────────────────────
	certPath, keyPath := generateSelfSignedCert(t, homeDir)
	t.Logf("Generated self-signed cert: %s  key: %s", certPath, keyPath)

	// ── First run: cluster up ─────────────────────────────────────────────────
	const (
		domain       = "example.test"
		adminEmail   = "admin@example.test"
	)
	upArgs := []string{
		"cluster", "up",
		"--domain=" + domain,
		"--cert=" + certPath,
		"--key=" + keyPath,
		"--openobserve-email=" + adminEmail,
	}

	t.Log("Running: pmcluster cluster up (first run)…")
	upOut, _, code := runCmdCtx(t, ctx, homeDir, upArgs...)
	if code != 0 {
		t.Fatalf("pmcluster cluster up exited %d:\n%s", code, upOut)
	}
	t.Logf("cluster up output:\n%s", upOut)

	// Ensure cluster is torn down even on test failure.
	t.Cleanup(func() {
		t.Log("TestClusterUp: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		out, _, _ := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge")
		t.Logf("cluster down output:\n%s", out)
	})

	// ── Assert first-run output ───────────────────────────────────────────────
	t.Run("first run output contains newly-created markers", func(t *testing.T) {
		// up.go prints "newly created (swarm secret …)" for each new cred.
		if !strings.Contains(upOut, "newly created") {
			t.Errorf("expected 'newly created' in first-run output; got:\n%s", upOut)
		}
		// printUpResult always emits this line.
		if !strings.Contains(upOut, "cluster up complete") {
			t.Errorf("expected 'cluster up complete' in output; got:\n%s", upOut)
		}
		// Bootstrap passwords block is shown for every cluster up (newly
		// created and pre-existing alike — see printUpResult).
		if !strings.Contains(upOut, "BOOTSTRAP CREDENTIALS") {
			t.Errorf("expected 'BOOTSTRAP CREDENTIALS' block in first-run output; got:\n%s", upOut)
		}
	})

	// ── Docker post-conditions ────────────────────────────────────────────────
	t.Run("docker stack ls shows infra, observability, backup", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "stack", "ls", "--format", "{{.Name}}")
		for _, stack := range []string{"infra", "observability", "backup"} {
			if !strings.Contains(out, stack) {
				t.Errorf("docker stack ls: stack %q not found; output:\n%s", stack, out)
			}
		}
	})

	t.Run("docker secret ls shows expected secrets", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "secret", "ls", "--format", "{{.Name}}")
		for _, secret := range []string{
			"admin_credentials",
			"cert",
			"key",
			"portainer_admin_password",
			"zo_root_user_password",
		} {
			if !strings.Contains(out, secret) {
				t.Errorf("docker secret ls: secret %q not found; output:\n%s", secret, out)
			}
		}
	})

	t.Run("docker config ls shows pmcluster configs", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "config", "ls", "--format", "{{.Name}}")
		for _, cfg := range []string{"pmcluster_otel_config", "pmcluster_traefik_dynamic"} {
			if !strings.Contains(out, cfg) {
				t.Errorf("docker config ls: config %q not found; output:\n%s", cfg, out)
			}
		}
	})

	t.Run("docker network ls shows overlay networks", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "network", "ls", "--filter", "driver=overlay", "--format", "{{.Name}}")
		for _, net := range []string{"traefik-net", "monitoring-net"} {
			if !strings.Contains(out, net) {
				t.Errorf("docker network ls: network %q not found; output:\n%s", net, out)
			}
		}
	})

	// ── credentials CLI ───────────────────────────────────────────────────────
	t.Run("credentials list shows three rows", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "credentials", "list")
		if code != 0 {
			t.Fatalf("pmcluster credentials list exited %d:\n%s", code, out)
		}
		// Header row + 3 data rows = at least 3 newlines after header.
		// Each data row contains a tab-separated name. We check for each name.
		for _, name := range []string{"traefik_dashboard", "portainer", "openobserve_admin"} {
			if !strings.Contains(out, name) {
				t.Errorf("credentials list: %q not found; output:\n%s", name, out)
			}
		}
	})

	t.Run("credentials show portainer prints non-empty password", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "credentials", "show", "portainer")
		if code != 0 {
			t.Fatalf("pmcluster credentials show portainer exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "password:") {
			t.Errorf("expected 'password:' line in credentials show output; got:\n%s", out)
		}
		// password: <value> — value must be non-empty
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "password:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
					t.Errorf("password line is empty: %q", line)
				}
				break
			}
		}
	})

	// ── Idempotency: second run ───────────────────────────────────────────────
	t.Run("second cluster up is idempotent (no newly-created lines)", func(t *testing.T) {
		t.Log("Running: pmcluster cluster up (second run — idempotency check)…")
		out2, _, code := runCmdCtx(t, ctx, homeDir, upArgs...)
		if code != 0 {
			t.Fatalf("pmcluster cluster up (second run) exited %d:\n%s", code, out2)
		}
		t.Logf("cluster up (idempotent) output:\n%s", out2)

		// On a re-run, no credential should be marked as "newly created".
		if strings.Contains(out2, "newly created") {
			t.Errorf("expected NO 'newly created' on second run; got:\n%s", out2)
		}
		// But the completion marker must still be present.
		if !strings.Contains(out2, "cluster up complete") {
			t.Errorf("expected 'cluster up complete' on second run; got:\n%s", out2)
		}
	})

	// ── Cleanup: cluster down --purge ─────────────────────────────────────────
	// This is tested here explicitly (in addition to the t.Cleanup above) to
	// verify the command exits 0 before the deferred cleanup runs.
	t.Run("cluster down --yes --purge exits 0", func(t *testing.T) {
		downOut, _, code := runCmdCtx(t, ctx, homeDir, "cluster", "down", "--yes", "--purge")
		if code != 0 {
			t.Fatalf("pmcluster cluster down --yes --purge exited %d:\n%s", code, downOut)
		}
		t.Logf("cluster down output:\n%s", downOut)
		// After purge, stacks should be gone.
		// Give Swarm a moment to remove stacks before querying.
		time.Sleep(3 * time.Second)
		stackOut, _ := dockerRunNoFail(ctx, "stack", "ls", "--format", "{{.Name}}")
		for _, stack := range []string{"infra", "observability", "backup"} {
			if strings.Contains(stackOut, stack) {
				t.Errorf("stack %q still listed after cluster down --purge; stack ls output:\n%s", stack, stackOut)
			}
		}
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ensureSwarmActive checks whether Swarm is already active. If not, it runs
// `docker swarm init --advertise-addr 127.0.0.1` and returns true so the
// caller knows to leave the Swarm in t.Cleanup.
func ensureSwarmActive(t *testing.T, ctx context.Context) (weInited bool) {
	t.Helper()
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 15*time.Second)
	defer inspectCancel()

	out, err := dockerRun(inspectCtx, "info", "--format", "{{.Swarm.LocalNodeState}}")
	if err == nil && strings.TrimSpace(out) == "active" {
		t.Log("Swarm already active; skipping docker swarm init")
		return false
	}

	t.Log("Swarm not active; running docker swarm init --advertise-addr 127.0.0.1")
	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()

	initOut, initErr := dockerRun(initCtx, "swarm", "init", "--advertise-addr", "127.0.0.1")
	if initErr != nil {
		t.Fatalf("docker swarm init: %v\n%s", initErr, initOut)
	}
	t.Log("docker swarm init: OK")
	return true
}

// generateSelfSignedCert creates a self-signed RSA-2048 certificate + key in
// dir using Go stdlib crypto only (no openssl). The cert has a 1-year validity,
// CN=example.test, and SANs: *.example.test.
// Returns the paths to the written cert and key PEM files.
func generateSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "example.test",
		},
		DNSNames:              []string{"*.example.test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}

	certPath = fmt.Sprintf("%s/tls.crt", dir)
	keyPath = fmt.Sprintf("%s/tls.key", dir)

	// Write cert
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("pem.Encode cert: %v", err)
	}
	certFile.Close()

	// Write key
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: privDER}); err != nil {
		t.Fatalf("pem.Encode key: %v", err)
	}
	keyFile.Close()

	return certPath, keyPath
}

// dockerRun executes a `docker <args...>` command with a timeout and returns
// combined stdout+stderr and any error.
func dockerRun(ctx context.Context, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = io.MultiWriter(&buf, os.Stdout)
	cmd.Stderr = io.MultiWriter(&buf, os.Stdout)
	err := cmd.Run()
	return buf.String(), err
}

// dockerRunNoFail is like dockerRun but ignores the error (for post-condition
// queries where absence is acceptable).
func dockerRunNoFail(ctx context.Context, args ...string) (string, error) {
	return dockerRun(ctx, args...)
}

// mustDockerRun runs docker and fails the test if it errors.
func mustDockerRun(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := dockerRun(cmdCtx, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// runCmdCtx is like runCmd but accepts a context (so we can cap individual
// pmcluster invocations within the overall test timeout).
func runCmdCtx(t *testing.T, ctx context.Context, homeDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer

	// Stream to both the buffer and os.Stdout so CI logs are visible.
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Stdout = io.MultiWriter(&stdoutBuf, os.Stdout)
	cmd.Stderr = io.MultiWriter(&stderrBuf, os.Stderr)
	cmd.Env = homeEnv(homeDir)

	err := cmd.Run()
	stdout = stdoutBuf.String()
	stderr = stderrBuf.String()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("cmd %v: unexpected error %v", args, err)
		}
	}
	return stdout, stderr, exitCode
}
