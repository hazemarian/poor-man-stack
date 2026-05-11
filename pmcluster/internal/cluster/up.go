package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

type UpInput struct {
	Domain                string
	CertPath              string
	KeyPath               string
	TraefikAdminUser      string // defaults to "admin"
	OpenObserveAdminEmail string
}

// UpResult includes plaintext passwords for any credentials newly minted
// on this run; existing ones are returned with NewlyCreated=false.
type UpResult struct {
	NewNetworks          []string
	NewSecrets           []string
	NewConfigs           []string
	StacksDeployed       []string
	BootstrapCredentials map[string]*ManagedCredential
}

type UpDeps struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   docker.Client
	Deployer StackDeployer
	Stdout   io.Writer // io.Discard in tests
}

// Up brings the cluster up end-to-end. Order matters: preflight →
// networks → TLS secrets → bootstrap creds → render configs → deploy
// stacks (infra → observability → backup).
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

	step("Preflight: Docker reachable, Swarm active, this node is a manager")
	if err := Preflight(ctx, deps.Docker); err != nil {
		return nil, err
	}

	step("Ensuring overlay networks")
	created, err := EnsureBundledNetworks(ctx, deps.Docker)
	if err != nil {
		return res, fmt.Errorf("ensure networks: %w", err)
	}
	res.NewNetworks = created

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
			// "Lost DB, kept Swarm" recovery: pmcluster minted a new
			// password but a Swarm secret with that name already existed.
			// Store and Swarm now disagree.
			fmt.Fprintf(out, "  ⚠ %-20s store updated, but Swarm secret %s pre-existed (passwords may diverge)\n", name, c.SwarmSecretName)
		case !c.NewlyCreated && c.SwarmSecretCreated:
			res.NewSecrets = append(res.NewSecrets, c.SwarmSecretName)
			fmt.Fprintf(out, "  ✓ %-20s already in store; Swarm secret %s recreated\n", name, c.SwarmSecretName)
		default:
			fmt.Fprintf(out, "  ✓ %-20s already present\n", name)
		}
	}

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
