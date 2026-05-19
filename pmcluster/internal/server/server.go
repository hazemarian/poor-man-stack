// Package server wires the chi HTTP router for the pmcluster daemon.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/api"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/webhook"
)

// Deps bundles the collaborators a fully-wired server needs. Optional
// fields cause their associated routes to be omitted when nil, so tests
// can wire a partial server.
type Deps struct {
	Lookup        auth.Lookup
	Docker        docker.Client
	Store         *store.Store
	DeployService *deploy.Service
	Cipher        *credentials.Cipher
	BackupTrigger api.BackupTrigger
}

func New(d Deps) http.Handler {
	r := chi.NewRouter()

	// RealIP first so later middleware sees the right address; Recoverer
	// last so panics become 500s instead of crashing the daemon.
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

	// otelhttp wraps the whole router so every request produces a span
	// + http.server.* metrics keyed by route template (chi populates
	// the route on the context, otelhttp picks it up via the
	// http.route attribute it sets after the chi pattern matches).
	return otelhttp.NewHandler(r, "pmcluster.http")
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled,
// then gracefully shuts down with a 10s deadline.
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
