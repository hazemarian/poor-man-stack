package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
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
}

type dockerCLIDeployer struct {
	envExtras []string
	stdout    io.Writer
	stderr    io.Writer
}

// NewDockerCLIDeployer streams the docker process's stdout/stderr to w
// (os.Stdout for live progress; nil to discard).
func NewDockerCLIDeployer(w io.Writer) StackDeployer {
	return &dockerCLIDeployer{stdout: w, stderr: w}
}

func (d *dockerCLIDeployer) DeployStack(ctx context.Context, name string, composeYAML []byte) error {
	// --with-registry-auth forwards ~/.docker/config.json to all nodes
	// (encrypted via swarm's own keys) so workers can pull private images.
	cmd := exec.CommandContext(ctx, "docker", "stack", "deploy",
		"--detach=true",
		"--resolve-image=always",
		"--with-registry-auth",
		"-c", "-",
		name,
	)
	cmd.Stdin = bytes.NewReader(composeYAML)
	cmd.Stdout = d.stdout
	cmd.Stderr = d.stderr
	if len(d.envExtras) > 0 {
		cmd.Env = append([]string{}, d.envExtras...)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker stack deploy %s: %w", name, err)
	}
	return nil
}

func (d *dockerCLIDeployer) RemoveStack(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "docker", "stack", "rm", name)
	cmd.Stdout = d.stdout
	cmd.Stderr = d.stderr
	if err := cmd.Run(); err != nil {
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
	cmd.Stdout = d.stdout
	cmd.Stderr = d.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker service update --force %s: %w", fullName, err)
	}
	return nil
}
