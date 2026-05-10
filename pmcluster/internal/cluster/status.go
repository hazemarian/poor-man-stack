package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// StatusReport captures everything `pmcluster cluster status` shows.
// Phase 2.2 keeps this lightweight: preflight result + counts.
// Phase 2.4+ may extend with per-service health (would need Docker SDK
// ServiceList + TaskList).
type StatusReport struct {
	Preflight       error // nil if all preflight checks pass
	NodeName        string
	ServerVersion   string
	SwarmState      string
	IsManager       bool
	NodeCount       int
	ManagerCount    int
}

// Status runs the same preflight checks as Up (read-only — no
// side effects) and returns a snapshot of cluster health.
func Status(ctx context.Context, d docker.Client) (*StatusReport, error) {
	preflight := Preflight(ctx, d)

	info, infoErr := d.Info(ctx)
	if infoErr != nil {
		return nil, fmt.Errorf("docker info: %w", infoErr)
	}
	return &StatusReport{
		Preflight:     preflight,
		NodeName:      info.Name,
		ServerVersion: info.ServerVersion,
		SwarmState:    info.SwarmLocalNodeState,
		IsManager:     info.SwarmControlAvailable,
		NodeCount:     info.SwarmNodes,
		ManagerCount:  info.SwarmManagers,
	}, nil
}
