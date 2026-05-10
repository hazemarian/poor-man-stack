// Package server wires the chi HTTP router for the pmcluster daemon.
//
// Routing layout:
//
//	GET  /health      — unauthenticated liveness
//	GET  /api/me      — authenticated; bearer token via users table
//	POST /webhook/... — Phase 4; HMAC-verified webhook receivers
//	... /api/...      — Phase 3+; stack/registry/credentials/nodes resources
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/api"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/webhook"
)

// Deps bundles the collaborators a fully-wired server needs. Constructed by
// `pmcluster serve`; tests can substitute fakes for each.
//
// Docker, Store, DeployService, and Cipher are optional so server tests can
// run without their respective deps. The route is omitted when the dep is nil.
type Deps struct {
	Lookup        auth.Lookup
	Docker        docker.Client
	Store         *store.Store
	DeployService *deploy.Service
	Cipher        *credentials.Cipher
	BackupTrigger api.BackupTrigger // optional; POST /api/backups returns 503 when nil
}

// New builds the chi router with all routes wired. Returned as http.Handler
// so callers can wrap with their own middleware (e.g. logging) if needed.
func New(d Deps) http.Handler {
	r := chi.NewRouter()

	// Standard middleware. RealIP first so subsequent middleware sees the
	// right address; Recoverer last so panics in any of our handlers are
	// logged and turned into 500s instead of crashing the daemon.
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/health", api.Health)

	// /webhook/{source} sits OUTSIDE /api — HMAC IS the auth, no Bearer.
	if d.Store != nil && d.Cipher != nil && d.DeployService != nil {
		(&webhook.Handler{
			Store:   d.Store,
			Cipher:  d.Cipher,
			Service: d.DeployService,
		}).Mount(r)
	}

	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Bearer(d.Lookup))
		r.Get("/me", api.Me)
		if d.Docker != nil {
			r.Get("/cluster/info", api.ClusterInfoHandler(d.Docker))
			r.Get("/nodes", api.NodesHandler(d.Docker))
		}
		if d.Store != nil && d.DeployService != nil {
			(&api.StacksHandler{
				Store:   d.Store,
				Service: d.DeployService,
			}).Mount(r)
		}
		if d.Store != nil {
			bh := &api.BackupsHandler{Store: d.Store, Trigger: d.BackupTrigger}
			bh.Mount(r)
			bh.MountStackScoped(r)
		}
	})

	return r
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled.
// On cancellation it gracefully shuts down with a 10s deadline.
//
// Returns nil on clean shutdown; non-nil on bind error or shutdown timeout.
func Run(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
		} else {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
