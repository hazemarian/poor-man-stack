package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// UpInput is everything `pmcluster cluster up` needs to bootstrap the
// cluster from a fresh manager. Sourced from CLI flags + persisted config.
type UpInput struct {
	Domain                string
	CertPath              string
	KeyPath               string
	TraefikAdminUser      string // default "admin" if empty
	OpenObserveAdminEmail string // operator's email
}

// UpResult summarises everything Up did, suitable for printing to the operator
// at the end of `pmcluster cluster up`. Includes the plaintext bootstrap
// passwords for any credentials that were newly created on this run.
type UpResult struct {
	NewNetworks         []string
	NewSecrets          []string
	NewConfigs          []string
	StacksDeployed      []string
	BootstrapCredentials map[string]*ManagedCredential
}

// UpDeps bundles the collaborators Up needs. Constructed by the CLI; each
// field can be substituted for tests.
type UpDeps struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   docker.Client
	Deployer StackDeployer

	// Stdout is where progress lines are written. Use os.Stdout from the CLI;
	// io.Discard from tests if you don't care about the output.
	Stdout io.Writer
}

// Up brings the cluster up end-to-end. Idempotent: every step uses the
// "ensure" pattern and never modifies existing state.
//
// Order matters:
//  1. Preflight (Docker daemon + Swarm + manager role)
//  2. Networks (traefik-net, monitoring-net)
//  3. TLS secrets from cert/key files (cert, key)
//  4. Bootstrap credentials (Traefik admin htpasswd, Portainer pwd, OpenObserve pwd)
//  5. Render configs (OTel pipeline + Traefik dynamic) and create as Docker configs
//  6. Deploy stacks (infra → observability → backup)
func Up(ctx context.Context, deps UpDeps, in UpInput) (*UpResult, error) {
	if err := validateUpInput(in); err != nil {
		return nil, err
	}
	out := io.Discard
	if deps.Stdout != nil {
		out = deps.Stdout
	}

	res := &UpResult{}
	step := func(label string) { fmt.Fprintf(out, "▶ %s\n", label) }

	// ── 1. Preflight ─────────────────────────────────────────────────────
	step("Preflight: Docker reachable, Swarm active, this node is a manager")
	if err := Preflight(ctx, deps.Docker); err != nil {
		return nil, err
	}

	// ── 2. Networks ──────────────────────────────────────────────────────
	step("Ensuring overlay networks")
	created, err := EnsureBundledNetworks(ctx, deps.Docker)
	if err != nil {
		return res, fmt.Errorf("ensure networks: %w", err)
	}
	res.NewNetworks = created

	// ── 3. TLS cert/key secrets from disk ────────────────────────────────
	step("Loading TLS cert/key into Swarm secrets")
	for _, t := range []struct{ name, path string }{
		{"cert", in.CertPath},
		{"key", in.KeyPath},
	} {
		isNew, err := EnsureSecretFromFile(ctx, deps.Docker, t.name, t.path)
		if err != nil {
			return res, fmt.Errorf("ensure secret %s from %s: %w", t.name, t.path, err)
		}
		if isNew {
			res.NewSecrets = append(res.NewSecrets, t.name)
		}
	}

	// ── 4. Bootstrap credentials ────────────────────────────────────────
	step("Bootstrapping managed credentials (Traefik / Portainer / OpenObserve)")
	credMgr := &CredentialsManager{
		Store:  deps.Store,
		Cipher: deps.Cipher,
		Docker: deps.Docker,
	}
	creds, err := credMgr.Bootstrap(ctx, BootstrapInput{
		TraefikAdminUser:      in.TraefikAdminUser,
		OpenObserveAdminEmail: in.OpenObserveAdminEmail,
	})
	if err != nil {
		return res, fmt.Errorf("bootstrap credentials: %w", err)
	}
	res.BootstrapCredentials = creds
	for name, c := range creds {
		switch {
		case c.NewlyCreated && c.SwarmSecretCreated:
			res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
			fmt.Fprintf(out, "  ✓ %-20s newly created (swarm secret %s)\n", name, c.SwarmSecretName)
		case c.NewlyCreated && !c.SwarmSecretCreated:
			// Lost-DB recovery scenario: pmcluster minted a new password,
			// but a Swarm secret with that name already existed (from a
			// prior install with a now-gone DB). The two now disagree.
			fmt.Fprintf(out, "  ⚠ %-20s store updated, but Swarm secret %s pre-existed (passwords may diverge)\n", name, c.SwarmSecretName)
		case !c.NewlyCreated && c.SwarmSecretCreated:
			res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
			fmt.Fprintf(out, "  ✓ %-20s already in store; Swarm secret %s recreated\n", name, c.SwarmSecretName)
		default:
			fmt.Fprintf(out, "  ✓ %-20s already present\n", name)
		}
	}

	// ── 5. Render Docker configs (OTel + Traefik dynamic) ───────────────
	step("Rendering and creating Docker configs (OTel pipeline, Traefik dynamic)")
	openobsCred := creds["openobserve_admin"]
	if openobsCred == nil {
		return res, fmt.Errorf("internal: openobserve_admin credential missing after bootstrap")
	}
	render := RenderInput{
		Domain:                   in.Domain,
		OpenObserveAdminEmail:    openobsCred.Username,
		OpenObserveAdminPassword: openobsCred.Password,
	}

	otelYAML, err := RenderOTelCollectorConfig(render)
	if err != nil {
		return res, err
	}
	if isNew, err := EnsureConfig(ctx, deps.Docker, "pmcluster_otel_config", otelYAML); err != nil {
		return res, err
	} else if isNew {
		res.NewConfigs = append(res.NewConfigs, "pmcluster_otel_config")
	}

	traefikYAML, err := RenderTraefikDynamic(render)
	if err != nil {
		return res, err
	}
	if isNew, err := EnsureConfig(ctx, deps.Docker, "pmcluster_traefik_dynamic", traefikYAML); err != nil {
		return res, err
	} else if isNew {
		res.NewConfigs = append(res.NewConfigs, "pmcluster_traefik_dynamic")
	}

	// ── 6. Deploy stacks ─────────────────────────────────────────────────
	step("Deploying stacks (infra → observability → backup)")
	for _, s := range []stackName{StackInfra, StackObservability, StackBackup} {
		composeYAML, err := LoadComposeFile(s, render)
		if err != nil {
			return res, fmt.Errorf("load compose for %s: %w", s, err)
		}
		fmt.Fprintf(out, "  ▶ docker stack deploy %s\n", s)
		if err := deps.Deployer.DeployStack(ctx, string(s), composeYAML); err != nil {
			return res, fmt.Errorf("deploy %s: %w", s, err)
		}
		res.StacksDeployed = append(res.StacksDeployed, string(s))
	}

	step("Cluster up complete.")
	return res, nil
}

// validateUpInput catches the obvious user mistakes early so we don't get a
// confusing failure five steps in.
func validateUpInput(in UpInput) error {
	if in.Domain == "" {
		return fmt.Errorf("--domain is required")
	}
	if in.CertPath == "" {
		return fmt.Errorf("--cert is required (path to TLS certificate)")
	}
	if in.KeyPath == "" {
		return fmt.Errorf("--key is required (path to TLS private key)")
	}
	if in.OpenObserveAdminEmail == "" {
		return fmt.Errorf("--openobserve-email is required (used as OpenObserve admin login)")
	}
	return nil
}
