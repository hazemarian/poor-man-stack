// Package cluster owns the lifecycle of the bundled Docker Swarm cluster
// (Traefik, Portainer, OpenObserve, OTel Collector, volume backup).
//
// It replaces the bash bin/setup.sh entirely:
//   - preflight.go : verify Docker is installed and Swarm is active+manager
//   - networks.go  : ensure overlay networks exist (idempotent)
//   - secrets.go   : ensure Docker Swarm secrets exist (idempotent, never modified)
//   - templates.go : embed and render OTel + Traefik dynamic configs
//   - stacks.go    : deploy infra/observability/backup stacks
//   - credentials.go : generate + persist random bootstrap credentials
//   - up.go        : orchestrate all of the above
//
// All operations are designed to be idempotent — re-running `pmcluster
// cluster up` should reconcile, never destroy or rotate.
package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// PreflightError describes a failed precondition with a remediation string
// suitable for printing directly to the operator. Callers can pattern-match
// on the embedded `Cause` if they want to react programmatically.
type PreflightError struct {
	Cause       error  // wrapped low-level error, may be nil for "wrong state" failures
	Remediation string // operator-facing hint, e.g. "run docker swarm init --advertise-addr <ip>"
}

func (e *PreflightError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%v\n\n%s", e.Cause, e.Remediation)
	}
	return e.Remediation
}

func (e *PreflightError) Unwrap() error { return e.Cause }

// Preflight runs every check the cluster bootstrap depends on, returning the
// first failure with a remediation string. Returns nil iff all checks pass.
//
// Checks (in order):
//  1. Docker daemon is reachable (Ping succeeds)
//  2. Local node is part of an active Swarm
//  3. Local node is a Swarm manager (control plane available)
//
// Each failure surfaces as a *PreflightError so callers (CLI, future API
// endpoints) can render the remediation consistently.
func Preflight(ctx context.Context, d docker.Client) error {
	if _, err := d.Ping(ctx); err != nil {
		return &PreflightError{
			Cause: err,
			Remediation: "Docker daemon is not reachable.\n" +
				"  • If Docker is not installed: https://docs.docker.com/engine/install/\n" +
				"  • If installed: start it (systemctl start docker, or open Docker Desktop)\n" +
				"  • Ensure your user can talk to the socket (group membership, DOCKER_HOST, etc.)",
		}
	}

	info, err := d.Info(ctx)
	if err != nil {
		return &PreflightError{
			Cause:       err,
			Remediation: "docker info failed; check daemon health.",
		}
	}

	if info.SwarmLocalNodeState != "active" {
		return &PreflightError{
			Cause: errors.New("Swarm not active on this node (state: " + info.SwarmLocalNodeState + ")"),
			Remediation: "Initialise Swarm before running pmcluster cluster up:\n" +
				"  docker swarm init --advertise-addr <this-node-ip>\n\n" +
				"pmcluster intentionally does NOT init Swarm — that decision " +
				"belongs to the operator.",
		}
	}

	if !info.SwarmControlAvailable {
		return &PreflightError{
			Cause: errors.New("this node is a Swarm worker, not a manager"),
			Remediation: "pmcluster cluster up must run on a Swarm manager.\n" +
				"  Workers should only run: docker swarm join --token <TOKEN> <MANAGER>:2377",
		}
	}

	return nil
}
