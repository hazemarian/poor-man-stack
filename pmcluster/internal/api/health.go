// Package api implements the pmcluster HTTP handlers — REST endpoints
// served by internal/server. Phase 1 covers /health and /api/me.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/buildinfo"
)

// Health is an unauthenticated liveness endpoint suitable for use by
// supervisors (brew services, systemd, k8s, load balancers).
//
// Returns 200 with a small JSON body. Does NOT touch the DB or Docker —
// liveness should not block on dependencies.
func Health(w http.ResponseWriter, _ *http.Request) {
	v, c, _ := buildinfo.Resolve()
	body := map[string]any{
		"status":  "ok",
		"version": v,
		"commit":  c,
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
