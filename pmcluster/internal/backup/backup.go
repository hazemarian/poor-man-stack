// Package backup triggers an on-demand offen backup via `docker exec`
// against the local container. Multi-node fan-out is out of scope; the
// nightly cron on every node still runs unchanged.
package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ServiceName matches the bundled backup-stack.yml (<stack>_<service>).
const ServiceName = "backup_volume-backup"

// LocalTrigger satisfies deploy.BackupTrigger via the package-level Trigger.
type LocalTrigger struct{}

func (LocalTrigger) Trigger(ctx context.Context) ([]string, error) {
	res, err := Trigger(ctx)
	if err != nil {
		if res != nil {
			return res.ArchivePaths, err
		}
		return nil, err
	}
	return res.ArchivePaths, nil
}

type Result struct {
	ArchivePaths []string
	Stdout       string
	Stderr       string
}

// Trigger runs `docker exec <offen> backup`. Caller should pass a ctx
// with a deadline (~5 min is plenty for typical volumes).
func Trigger(ctx context.Context) (*Result, error) {
	containerID, err := findLocalContainer(ctx)
	if err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "exec", containerID, "backup")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &Result{Stdout: stdout.String(), Stderr: stderr.String()},
			fmt.Errorf("docker exec %s backup: %w (%s)", containerID, err, strings.TrimSpace(stderr.String()))
	}
	return &Result{
		ArchivePaths: parseArchivePaths(stdout.String()),
		Stdout:       stdout.String(),
		Stderr:       stderr.String(),
	}, nil
}

// findLocalContainer filters by service-name label so a renamed or
// replaced container is still found by its provenance.
func findLocalContainer(ctx context.Context) (string, error) {
	timeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(timeout, "docker", "ps",
		"--filter", "label=com.docker.swarm.service.name="+ServiceName,
		"--filter", "status=running",
		"--format", "{{.ID}}",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker ps: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", errors.New("no offen backup container running on this host (is the backup stack deployed?)")
	}
	// Defensive: mode=global should give one container per node, but trim
	// to the first line if Docker ever returns more.
	if idx := strings.IndexByte(id, '\n'); idx >= 0 {
		id = id[:idx]
	}
	return id, nil
}

// parseArchivePaths is permissive — offen's wording has shifted across
// versions, so we match any /archive/*.tar.gz token in stdout.
func parseArchivePaths(stdout string) []string {
	var out []string
	for _, tok := range strings.Fields(stdout) {
		tok = strings.TrimRight(tok, ".,;:")
		if strings.HasPrefix(tok, "/archive/") && strings.HasSuffix(tok, ".tar.gz") {
			out = append(out, tok)
		}
	}
	return out
}
