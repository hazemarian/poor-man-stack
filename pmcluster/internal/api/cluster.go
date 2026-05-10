package api

import (
	"net/http"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// ClusterInfoHandler returns the docker info summary that pmcluster cares
// about. Phase 1.6 smoke endpoint that proves the daemon socket works;
// Phase 4 will add /api/nodes (multi-node listing) and /api/cluster/status
// (does the cluster look healthy from pmcluster's perspective).
func ClusterInfoHandler(d docker.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := d.Info(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"node_name":       info.Name,
			"server_version":  info.ServerVersion,
			"os":              info.OperatingSystem,
			"arch":            info.Architecture,
			"cpus":            info.NCPU,
			"memory_bytes":    info.MemTotal,
			"swarm": map[string]any{
				"state":              info.SwarmLocalNodeState,
				"control_available":  info.SwarmControlAvailable,
				"managers":           info.SwarmManagers,
				"nodes":              info.SwarmNodes,
			},
		})
	}
}
