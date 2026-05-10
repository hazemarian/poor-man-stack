package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// StackDeployer applies a compose file to the swarm under a given stack name.
// Production impl shells out to `docker stack deploy -c - <name>`; tests
// substitute a fake.
//
// Per the plan's Open Question 3 resolution: shelling out to the docker CLI
// is the pragmatic choice for Phase 2. The Docker Go SDK has no high-level
// stack-deploy primitive; reimplementing the docker CLI's stack-deploy
// semantics (compose parsing → per-service ServiceCreate/Update + secrets
// + networks + configs orchestration) is hundreds of lines of code.
type StackDeployer interface {
	DeployStack(ctx context.Context, name string, composeYAML []byte) error
	RemoveStack(ctx context.Context, name string) error

	// ForceUpdateService restarts all tasks of a service in place. Used by
	// credentials rotate to make tasks re-mount a freshly-created secret.
	// The fully-qualified service name is "<stack>_<service>" (e.g.
	// "infra_traefik").
	ForceUpdateService(ctx context.Context, fullName string) error
}

// dockerCLIDeployer is the production StackDeployer.
//
// We require `docker` CLI to be on PATH on the manager. This is a soft
// requirement — operators who installed Docker via official packages get
// the CLI for free.
type dockerCLIDeployer struct {
	// envExtras lets the caller layer on extra env vars (DOCKER_HOST, etc.)
	// for testing or unusual deployments. Defaults to inherited environment.
	envExtras []string

	// stdout/stderr capture is exposed so callers can surface deploy output
	// to the operator. Defaults to discard if nil — set to os.Stdout to
	// stream live progress.
	stdout io.Writer
	stderr io.Writer
}

// NewDockerCLIDeployer returns a StackDeployer that shells out to the
// docker CLI. Streams stdout/stderr of the docker process to w (use
// os.Stdout for live progress; nil to discard).
func NewDockerCLIDeployer(w io.Writer) StackDeployer {
	return &dockerCLIDeployer{stdout: w, stderr: w}
}

func (d *dockerCLIDeployer) DeployStack(ctx context.Context, name string, composeYAML []byte) error {
	// --with-registry-auth: forward the manager's registry credentials from
	//   ~/.docker/config.json to all swarm nodes (encrypted via the swarm's
	//   own keys) so workers can pull private images. Cheap on every deploy;
	//   no-op when there are no logged-in registries.
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
		// Inherit current env, then layer extras.
		// (cmd.Env nil means inherit; once we set it we replace, so build the
		// full slice. We don't currently use envExtras outside tests.)
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
