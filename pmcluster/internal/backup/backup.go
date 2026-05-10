// Package backup wraps the offen/docker-volume-backup container so pmcluster
// can trigger an on-demand backup before a deploy or on operator request.
//
// offen runs as a global Swarm service ("backup_volume-backup"); each node
// gets one task. We can trigger a backup by `docker exec`-ing into the
// local task with the `backup` command — offen's CLI rolls a fresh
// archive into /var/backups/docker-volumes/.
//
// pmcluster only triggers the LOCAL container (the one on the manager
// where pmcluster is installed). Multi-node fan-out is out of scope; the
// nightly cron schedule on every node still runs unchanged.
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

// ServiceName is the Swarm service name offen runs under, derived from
// the bundled backup-stack.yml (`<stack>_<service>` = "backup_volume-backup").
const ServiceName = "backup_volume-backup"

// LocalTrigger satisfies deploy.BackupTrigger via the package-level
// Trigger function; its only state is implicit (the local docker socket).
type LocalTrigger struct{}

// Trigger calls the package-level Trigger and returns just the archive
// paths (the rest of Result is for richer callers).
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

// Result is what Trigger reports back: the archive paths offen wrote (parsed
// from its stdout) and any error it surfaced.
type Result struct {
	ArchivePaths []string
	Stdout       string
	Stderr       string
}

// Trigger runs `docker exec <local-offen-container> backup` and returns
// the parsed archive paths. Returns an error if no local container is
// running or the exec exits non-zero.
//
// Timeout: the offen exec is bound to ctx — caller should pass a context
// with a sensible deadline (5 min is plenty for typical volumes).
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

// findLocalContainer returns the container id of the offen task running
// on this host. We filter by service-name label so a renamed/replaced
// container is still found by its provenance, not its name.
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
	// Possible multi-line output if Docker ever returns >1 (shouldn't, mode=global => 1 per node).
	if idx := strings.IndexByte(id, '\n'); idx >= 0 {
		id = id[:idx]
	}
	return id, nil
}

// parseArchivePaths walks offen's stdout looking for archive filenames.
// offen logs lines like "Successfully created backup at /archive/backup-...tar.gz"
// (the exact phrasing has shifted across versions). We're permissive:
// any token starting with "/archive/" and ending in ".tar.gz" is captured.
func parseArchivePaths(stdout string) []string {
	var out []string
	for _, tok := range strings.Fields(stdout) {
		// Strip trailing punctuation that sometimes follows a filename in log lines.
		tok = strings.TrimRight(tok, ".,;:")
		if strings.HasPrefix(tok, "/archive/") && strings.HasSuffix(tok, ".tar.gz") {
			out = append(out, tok)
		}
	}
	return out
}
