package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

type DownInput struct {
	// Purge removes pmcluster-managed secrets, configs, and the two
	// overlay networks. SQLite state is never touched.
	Purge bool
}

type DownResult struct {
	StacksRemoved   []string
	SecretsRemoved  []string
	ConfigsRemoved  []string
	NetworksRemoved []string
}

type DownDeps struct {
	Docker   docker.Client
	Deployer StackDeployer
	Stdout   io.Writer
}

// pmclusterManagedSecrets is enumerated explicitly (not "anything with
// the pmcluster label") so a mislabelled operator secret can't be
// accidentally purged.
var pmclusterManagedSecrets = []string{
	"admin_credentials",
	"portainer_admin_password",
	"zo_root_user_password",
	"cert",
	"key",
}

var pmclusterManagedConfigs = []string{
	"pmcluster_otel_config",
	"pmcluster_traefik_dynamic",
}

var pmclusterManagedNetworks = []string{
	"traefik-net",
	"monitoring-net",
}

// Down is idempotent — missing resources are no-ops.
func Down(ctx context.Context, deps DownDeps, in DownInput) (*DownResult, error) {
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}
	res := &DownResult{}
	step := func(label string) { fmt.Fprintf(out, "▶ %s\n", label) }

	// Stacks first — removing referenced secrets/configs/networks while
	// services still hold them would error.
	step("Removing stacks (infra, observability, backup)")
	for _, s := range []string{"infra", "observability", "backup"} {
		if err := deps.Deployer.RemoveStack(ctx, s); err != nil {
			fmt.Fprintf(out, "  ⚠ stack rm %s: %v\n", s, err)
			continue
		}
		res.StacksRemoved = append(res.StacksRemoved, s)
	}

	if !in.Purge {
		step("Cluster down complete (secrets/configs/networks preserved — pass --purge to wipe)")
		return res, nil
	}

	// Let Swarm release references; otherwise secret/config removal races
	// against still-terminating tasks.
	fmt.Fprintln(out, "  Waiting briefly for stack teardown to settle…")
	waitTeardownSettle(ctx)

	step("Purging pmcluster-managed Swarm secrets")
	for _, name := range pmclusterManagedSecrets {
		if err := deps.Docker.SecretRemove(ctx, name); err != nil {
			fmt.Fprintf(out, "  ⚠ %s: %v\n", name, err)
			continue
		}
		res.SecretsRemoved = append(res.SecretsRemoved, name)
	}

	step("Purging pmcluster-managed Docker configs")
	for _, name := range pmclusterManagedConfigs {
		if err := deps.Docker.ConfigRemove(ctx, name); err != nil {
			fmt.Fprintf(out, "  ⚠ %s: %v\n", name, err)
			continue
		}
		res.ConfigsRemoved = append(res.ConfigsRemoved, name)
	}

	step("Purging pmcluster-managed overlay networks")
	for _, name := range pmclusterManagedNetworks {
		if err := deps.Docker.NetworkRemove(ctx, name); err != nil {
			fmt.Fprintf(out, "  ⚠ %s: %v\n", name, err)
			continue
		}
		res.NetworksRemoved = append(res.NetworksRemoved, name)
	}

	step("Purge complete (SQLite at ~/.pmcluster preserved — delete manually if desired)")
	return res, nil
}

func waitTeardownSettle(ctx context.Context) {
	const settleSeconds = 5
	timer := newTimer(settleSeconds)
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
