// Package docker wraps the Docker Engine SDK behind a small interface so the
// rest of pmcluster (cluster bootstrap, manifest deploy, registry, nodes)
// can be tested with fakes instead of needing a real /var/run/docker.sock.
//
// Methods grow phase by phase:
//   - Phase 1.6: Ping, Info
//   - Phase 2:   NetworkExists/Create, SecretExists/Create (cluster bootstrap)
//   - Phase 3:   ServiceCreate/Update for manifest deploys
//   - Phase 4:   NodeList, ServiceLogs, RegistryAuth
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// Client is the contract pmcluster needs from a Docker daemon. Subset of
// the official SDK; tests inject a fake.
type Client interface {
	Ping(ctx context.Context) (Ping, error)
	Info(ctx context.Context) (Info, error)

	// NetworkExists returns true iff a network with the given name is present.
	// Does NOT distinguish between "not found" and "transient API error" —
	// both surface as a non-nil error from the wrapped SDK call, except for
	// "not found" which we collapse to (false, nil).
	NetworkExists(ctx context.Context, name string) (bool, error)

	// NetworkCreate creates an overlay/attachable network. Idempotency is
	// the caller's job (use NetworkExists first).
	NetworkCreate(ctx context.Context, spec NetworkSpec) error

	// SecretExists is the secret analogue of NetworkExists.
	SecretExists(ctx context.Context, name string) (bool, error)

	// SecretCreate creates a Swarm secret. Idempotency is the caller's job.
	SecretCreate(ctx context.Context, spec SecretSpec) error

	// ConfigExists / ConfigCreate are the Docker-config analogues of the
	// secret methods. Configs are immutable, replicated to every node by
	// Swarm, and used here for the OTel pipeline + Traefik dynamic configs.
	ConfigExists(ctx context.Context, name string) (bool, error)
	ConfigCreate(ctx context.Context, spec ConfigSpec) error

	// Removal methods used by `pmcluster cluster down --purge`. Each returns
	// nil if the resource doesn't exist (idempotent teardown).
	SecretRemove(ctx context.Context, name string) error
	ConfigRemove(ctx context.Context, name string) error
	NetworkRemove(ctx context.Context, name string) error

	// Node management (Phase 4.C). NodeList returns one Node per swarm
	// member; JoinTokens returns the current Worker/Manager join tokens.
	NodeList(ctx context.Context) ([]Node, error)
	JoinTokens(ctx context.Context) (JoinTokens, error)

	Close() error
}

// Ping is a minimal liveness response from the daemon.
type Ping struct {
	APIVersion   string
	OSType       string
	Experimental bool
}

// Info is the subset of `docker info` pmcluster cares about.
type Info struct {
	Name                  string
	ServerVersion         string
	OperatingSystem       string
	Architecture          string
	NCPU                  int
	MemTotal              int64
	SwarmLocalNodeState   string // "inactive" | "pending" | "active" | "error" | "locked"
	SwarmControlAvailable bool   // true on a manager
	SwarmManagers         int
	SwarmNodes            int
}

// NetworkSpec describes an overlay network for cluster bootstrap.
// Driver defaults to "overlay" if empty.
type NetworkSpec struct {
	Name       string
	Driver     string
	Attachable bool
}

// SecretSpec describes a Docker Swarm secret. Labels are applied so
// pmcluster-managed secrets can be distinguished from operator-created ones
// (handy for `cluster down --purge`).
type SecretSpec struct {
	Name   string
	Data   []byte
	Labels map[string]string
}

// ConfigSpec is the Docker-config analogue of SecretSpec. Configs are
// non-sensitive bytes (e.g. rendered YAML) — Swarm distributes them to
// every node automatically. We use them for the OTel and Traefik dynamic
// configs that bundled services need at known paths.
type ConfigSpec struct {
	Name   string
	Data   []byte
	Labels map[string]string
}

// Node is the pmcluster-shaped subset of swarm.Node — one row per swarm
// member returned by `docker node ls`.
type Node struct {
	ID            string
	Hostname      string
	Role          string // "manager" | "worker"
	Availability  string // "active" | "pause" | "drain"
	Status        string // "ready" | "down" | "unknown" | …
	IsLeader      bool
	EngineVersion string
	Address       string // node's advertise address ("ip:2377"); blank for non-managers
	CreatedAt     int64  // unix seconds
	UpdatedAt     int64
}

// JoinTokens are the worker / manager join tokens currently active in the
// Swarm. Returned verbatim from the daemon — operators paste them into
// `docker swarm join` on each new node.
type JoinTokens struct {
	Worker  string
	Manager string
}

// realClient adapts the upstream `*client.Client` to our Client interface.
type realClient struct {
	c *client.Client
}

// New returns a Client wired to the local Docker daemon (auto-detects
// /var/run/docker.sock or the DOCKER_HOST env var).
func New() (Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &realClient{c: c}, nil
}

func (r *realClient) Close() error { return r.c.Close() }

func (r *realClient) Ping(ctx context.Context) (Ping, error) {
	p, err := r.c.Ping(ctx)
	if err != nil {
		return Ping{}, fmt.Errorf("docker ping: %w", err)
	}
	return Ping{
		APIVersion:   p.APIVersion,
		OSType:       p.OSType,
		Experimental: p.Experimental,
	}, nil
}

func (r *realClient) Info(ctx context.Context) (Info, error) {
	i, err := r.c.Info(ctx)
	if err != nil {
		return Info{}, fmt.Errorf("docker info: %w", err)
	}
	return Info{
		Name:                  i.Name,
		ServerVersion:         i.ServerVersion,
		OperatingSystem:       i.OperatingSystem,
		Architecture:          i.Architecture,
		NCPU:                  i.NCPU,
		MemTotal:              i.MemTotal,
		SwarmLocalNodeState:   string(i.Swarm.LocalNodeState),
		SwarmControlAvailable: i.Swarm.ControlAvailable,
		SwarmManagers:         i.Swarm.Managers,
		SwarmNodes:            i.Swarm.Nodes,
	}, nil
}

func (r *realClient) NetworkExists(ctx context.Context, name string) (bool, error) {
	_, err := r.c.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) || isNotFoundString(err) {
		return false, nil
	}
	return false, fmt.Errorf("network inspect %s: %w", name, err)
}

func (r *realClient) NetworkCreate(ctx context.Context, spec NetworkSpec) error {
	driver := spec.Driver
	if driver == "" {
		driver = "overlay"
	}
	_, err := r.c.NetworkCreate(ctx, spec.Name, network.CreateOptions{
		Driver:     driver,
		Attachable: spec.Attachable,
	})
	if err != nil {
		return fmt.Errorf("network create %s: %w", spec.Name, err)
	}
	return nil
}

func (r *realClient) SecretExists(ctx context.Context, name string) (bool, error) {
	_, _, err := r.c.SecretInspectWithRaw(ctx, name)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) || isNotFoundString(err) {
		return false, nil
	}
	return false, fmt.Errorf("secret inspect %s: %w", name, err)
}

func (r *realClient) SecretCreate(ctx context.Context, spec SecretSpec) error {
	annotations := swarm.Annotations{Name: spec.Name}
	if len(spec.Labels) > 0 {
		annotations.Labels = spec.Labels
	}
	_, err := r.c.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: annotations,
		Data:        spec.Data,
	})
	if err != nil {
		return fmt.Errorf("secret create %s: %w", spec.Name, err)
	}
	return nil
}

func (r *realClient) ConfigExists(ctx context.Context, name string) (bool, error) {
	_, _, err := r.c.ConfigInspectWithRaw(ctx, name)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) || isNotFoundString(err) {
		return false, nil
	}
	return false, fmt.Errorf("config inspect %s: %w", name, err)
}

func (r *realClient) ConfigCreate(ctx context.Context, spec ConfigSpec) error {
	annotations := swarm.Annotations{Name: spec.Name}
	if len(spec.Labels) > 0 {
		annotations.Labels = spec.Labels
	}
	_, err := r.c.ConfigCreate(ctx, swarm.ConfigSpec{
		Annotations: annotations,
		Data:        spec.Data,
	})
	if err != nil {
		return fmt.Errorf("config create %s: %w", spec.Name, err)
	}
	return nil
}

// idempotentRemove turns "not found" into nil so teardown loops don't bail
// on the first missing resource.
func idempotentRemove(err error, kind, name string) error {
	if err == nil {
		return nil
	}
	if errdefs.IsNotFound(err) || isNotFoundString(err) {
		return nil
	}
	return fmt.Errorf("%s remove %s: %w", kind, name, err)
}

func (r *realClient) SecretRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.SecretRemove(ctx, name), "secret", name)
}

func (r *realClient) ConfigRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.ConfigRemove(ctx, name), "config", name)
}

func (r *realClient) NetworkRemove(ctx context.Context, name string) error {
	return idempotentRemove(r.c.NetworkRemove(ctx, name), "network", name)
}

func (r *realClient) NodeList(ctx context.Context) ([]Node, error) {
	nodes, err := r.c.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker node ls: %w", err)
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		var addr string
		if n.ManagerStatus != nil {
			addr = n.ManagerStatus.Addr
		}
		out = append(out, Node{
			ID:            n.ID,
			Hostname:      n.Description.Hostname,
			Role:          string(n.Spec.Role),
			Availability:  string(n.Spec.Availability),
			Status:        string(n.Status.State),
			IsLeader:      n.ManagerStatus != nil && n.ManagerStatus.Leader,
			EngineVersion: n.Description.Engine.EngineVersion,
			Address:       addr,
			CreatedAt:     n.CreatedAt.Unix(),
			UpdatedAt:     n.UpdatedAt.Unix(),
		})
	}
	return out, nil
}

func (r *realClient) JoinTokens(ctx context.Context) (JoinTokens, error) {
	sw, err := r.c.SwarmInspect(ctx)
	if err != nil {
		return JoinTokens{}, fmt.Errorf("docker swarm inspect: %w", err)
	}
	return JoinTokens{
		Worker:  sw.JoinTokens.Worker,
		Manager: sw.JoinTokens.Manager,
	}, nil
}

// isNotFoundString is a fallback "not found" detector for older daemon
// versions where errdefs.IsNotFound doesn't recognise the error. Belt-and-
// braces; can be removed when we know the minimum daemon version supports
// the typed errors.
func isNotFoundString(err error) bool {
	if err == nil {
		return false
	}
	var msg []byte
	msg = append(msg, err.Error()...)
	return bytes.Contains(bytes.ToLower(msg), []byte("not found")) ||
		bytes.Contains(bytes.ToLower(msg), []byte("no such")) ||
		errors.Is(err, errNotFound)
}

// errNotFound is unused but reserved — leaves a hook for tests to assert
// "the wrapper returned NotFound" if we ever expose it.
var errNotFound = errors.New("not found")
