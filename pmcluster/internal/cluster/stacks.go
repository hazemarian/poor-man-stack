package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// StackDeployer applies a compose file to the swarm under a given stack
// name. Production impl shells out to the `docker` CLI because the SDK
// has no high-level stack-deploy primitive.
type StackDeployer interface {
	DeployStack(ctx context.Context, name string, composeYAML []byte) error
	RemoveStack(ctx context.Context, name string) error

	// ForceUpdateService restarts all tasks of a service in place; used to
	// make tasks re-mount a freshly-rotated secret. fullName is "<stack>_<service>".
	ForceUpdateService(ctx context.Context, fullName string) error

	// PruneContainers removes stopped containers older than the given
	// duration (e.g. 10m). Call after DeployStack to prevent stale task
	// containers from accumulating disk space.
	PruneContainers(ctx context.Context, olderThan string) error
}

type dockerCLIDeployer struct {
	envExtras []string
	stdout    io.Writer
	stderr    io.Writer
}

// NewDockerCLIDeployer streams the docker process's stdout/stderr to w
// (os.Stdout for live progress; nil to discard).  On failure the full
// captured output is attached to the error so callers (REST/webhook/CLI)
// can diagnose without SSH-ing into the host.
func NewDockerCLIDeployer(w io.Writer) StackDeployer {
	return &dockerCLIDeployer{stdout: w, stderr: w}
}

// runWithOutput captures combined stdout+stderr; always returns the
// captured output (trimmed) so callers can inspect it on both success
// and failure.
func (d *dockerCLIDeployer) runWithOutput(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer
	// Tee: live progress to the caller's writer AND capture for inspection.
	if d.stdout != nil {
		cmd.Stdout = io.MultiWriter(&buf, d.stdout)
		cmd.Stderr = io.MultiWriter(&buf, d.stderr)
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	if len(d.envExtras) > 0 {
		cmd.Env = append([]string{}, d.envExtras...)
	}
	err := cmd.Run()
	return trimOutput(buf.String()), err
}

// trimOutput strips trailing whitespace so error messages aren't padded
// with blank lines.
func trimOutput(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func (d *dockerCLIDeployer) DeployStack(ctx context.Context, name string, composeYAML []byte) error {
	// --with-registry-auth forwards ~/.docker/config.json to all nodes
	// (encrypted via swarm's own keys) so workers can pull private images.
	// --resolve-image=always ensures new image digests are picked up
	// even when the tag hasn't changed (e.g. 'latest' re-pushed).
	cmd := exec.CommandContext(ctx, "docker", "stack", "deploy",
		"--detach=true",
		"--resolve-image=always",
		"--with-registry-auth",
		"-c", "-",
		name,
	)
	cmd.Stdin = bytes.NewReader(composeYAML)
	out, err := d.runWithOutput(cmd)
	if err != nil {
		if out != "" {
			return fmt.Errorf("docker stack deploy %s: %s", name, out)
		}
		return fmt.Errorf("docker stack deploy %s: %w", name, err)
	}

	// Force a rolling update for every service in the stack so new images
	// take effect even when the service config hasn't changed (Docker
	// Swarm sometimes skips the update when only the image digest differs).
	if err := d.forceUpdateStackServices(ctx, name); err != nil {
		return err
	}

	return nil
}

// forceUpdateStackServices lists services belonging to a stack and
// force-restarts them. Replicated services get `docker service update
// --force`; run_once jobs (which become terminal after completion) get
// removed before stack deploy so they are re-created as fresh tasks.
func (d *dockerCLIDeployer) forceUpdateStackServices(ctx context.Context, stackName string) error {
	listCmd := exec.CommandContext(ctx, "docker", "stack", "services",
		"--format", "{{.Name}}", stackName)
	listOut, err := d.runWithOutput(listCmd)
	if err != nil {
		return fmt.Errorf("docker stack services %s: %s", stackName, listOut)
	}

	for _, fullName := range strings.Split(strings.TrimSpace(listOut), "\n") {
		fullName = strings.TrimSpace(fullName)
		if fullName == "" {
			continue
		}
		if err := d.ForceUpdateService(ctx, fullName); err != nil {
			return err
		}
	}
	return nil
}

func (d *dockerCLIDeployer) RemoveStack(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "docker", "stack", "rm", name)
	out, err := d.runWithOutput(cmd)
	if err != nil {
		if out != "" {
			return fmt.Errorf("docker stack rm %s: %s", name, out)
		}
		return fmt.Errorf("docker stack rm %s: %w", name, err)
	}
	return nil
}

func (d *dockerCLIDeployer) ForceUpdateService(ctx context.Context, fullName string) error {
	cmd := exec.CommandContext(ctx, "docker", "service", "update",
		"--force",
		"--detach=true",
		fullName,
	)
	out, err := d.runWithOutput(cmd)
	if err != nil {
		if out != "" {
			return fmt.Errorf("docker service update --force %s: %s", fullName, out)
		}
		return fmt.Errorf("docker service update --force %s: %w", fullName, err)
	}
	return nil
}

func (d *dockerCLIDeployer) PruneContainers(ctx context.Context, olderThan string) error {
	cmd := exec.CommandContext(ctx, "docker", "container", "prune",
		"--force",
		"--filter", "until="+olderThan,
	)
	out, err := d.runWithOutput(cmd)
	if err != nil {
		if out != "" {
			return fmt.Errorf("docker container prune: %s", out)
		}
		return fmt.Errorf("docker container prune: %w", err)
	}
	return nil
}
