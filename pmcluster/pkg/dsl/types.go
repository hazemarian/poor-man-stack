// Package dsl defines the public schema for pmcluster's higher-level
// application manifest. Customers write a small DSL document; pmcluster
// translates it into a verbose Docker Swarm Compose YAML and applies it.
//
// The translator (internal/manifest) auto-injects:
//   - traefik-net + monitoring-net membership for services with `expose:`
//   - Traefik labels (router, entrypoint, TLS, service port)
//   - Standard service labels (service, application, environment, version)
//   - networks block (shared as external, ${app}-net as inline overlay)
//   - external secrets block
//   - Default restart/update policies when omitted
//
// The DSL uses JSON tags throughout; sigs.k8s.io/yaml decodes YAML by way
// of JSON, which gives us strict unknown-field rejection and consistent
// camelCase ↔ snake_case behaviour.
package dsl

// App is the top-level manifest. Exactly one App per file.
type App struct {
	// Identity & environment.
	Name    string `json:"app"`     // required: stack name (also used in network/label substitution)
	Env     string `json:"env"`     // required: e.g. "production", "staging"
	Domain  string `json:"domain"`  // required: base domain — substituted as ${domain} in Expose.Host
	Version string `json:"version"` // optional: image tag — substituted as ${version}; defaults to "latest"

	// Image / registry shorthand. If set, services can use `image: ${registry}/...:${version}`.
	Registry string `json:"registry,omitempty"`

	// Repo URL for audit/UI. NOT fetched — pmcluster never reads from git.
	RepoURL string `json:"repo_url,omitempty"`

	// Optional environment file (relative path to a stack.env-style file).
	// Sent alongside the manifest in a multi-part body; Phase 3 wires this
	// via a separate API field rather than reading from disk.
	EnvFile string `json:"env_file,omitempty"`

	// External Swarm secrets the services may reference. Listed at the App
	// level so the translator can emit a top-level `secrets:` block with
	// `external: true` for each.
	Secrets []string `json:"secrets,omitempty"`

	// Top-level volumes: named local volumes the services may reference.
	Volumes []string `json:"volumes,omitempty"`

	// Services keyed by name (stable order preserved by the parser via the
	// raw YAML node walk; Go maps are unordered, so we re-sort by name in
	// the translator to keep golden files stable).
	Services map[string]*Service `json:"services"`

	// BackupBeforeDeploy: if true, pmcluster triggers an offen volume backup
	// on the local node BEFORE running `docker stack deploy`. The deploy
	// proceeds even if the backup fails (so a flaky backup container can't
	// block urgent deploys); the failure is recorded in the backups audit
	// table and surfaced loud in the deploy output.
	BackupBeforeDeploy bool `json:"backup_before_deploy,omitempty"`
}

// Service is one of the workloads inside a stack.
type Service struct {
	// Image — required. Supports interpolation: ${app}, ${env}, ${version},
	// ${registry}, ${env:VAR}.
	Image string `json:"image"`

	// Replicas: how many tasks of this service to run. Default: 1.
	// Ignored when RunOnce is true.
	Replicas *int `json:"replicas,omitempty"`

	// RunOnce: a one-shot service that runs to completion and is not
	// restarted. Translates to restart_policy: condition: none.
	// Useful for migrations.
	RunOnce bool `json:"run_once,omitempty"`

	// Placement constraint (Phase 3 supports just one common case).
	// Allowed values: "manager", "worker", "" (any).
	Placement string `json:"placement,omitempty"`

	// Command and entrypoint overrides (rarely needed; image usually defines them).
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Inline environment variables. Merged with the App's EnvFile if both set.
	Env map[string]string `json:"env,omitempty"`

	// Volumes the service mounts. Format: "name:path" or "/host/path:/container/path".
	Volumes []string `json:"volumes,omitempty"`

	// Secrets the service mounts (subset of App.Secrets).
	Secrets []string `json:"secrets,omitempty"`

	// Expose: if set, the translator wires Traefik labels, traefik-net, and
	// HTTPS routing. Omit for internal-only services.
	Expose *Expose `json:"expose,omitempty"`

	// Healthcheck: shorthand or full-form. See Healthcheck doc.
	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`

	// Update policy. Defaults: parallelism=1, delay=10s, order=start-first.
	Update *Update `json:"update,omitempty"`
}

// Expose declares that a service is reachable via Traefik over HTTPS.
type Expose struct {
	// Port the container listens on (inside the container; Traefik routes to it).
	Port int `json:"port"`

	// Host: full FQDN (typically uses ${app} and ${domain} substitution).
	// Example: "api.${app}.${domain}".
	Host string `json:"host"`
}

// Healthcheck is either a shorthand string OR a full-form struct. The
// parser populates Type when it sees a string; otherwise the explicit
// fields are used as-is.
type Healthcheck struct {
	// Type is a shorthand triggering a stock check:
	//   - "pg_isready"   → CMD-SHELL pg_isready -U $POSTGRES_USER -d $POSTGRES_DB
	//   - "http"         → wget -q --spider http://localhost:<port>/<Path>
	// If empty, the explicit Test/Interval/etc. fields are used directly
	// (Compose-spec semantics).
	Type string `json:"type,omitempty"`

	// Path used when Type="http". Defaults to "/" if Type="http" and Path is empty.
	Path string `json:"path,omitempty"`

	// Compose-spec passthrough fields (used when Type is empty).
	Test     []string `json:"test,omitempty"`
	Interval string   `json:"interval,omitempty"` // e.g. "10s"
	Timeout  string   `json:"timeout,omitempty"`
	Retries  int      `json:"retries,omitempty"`
}

// Update controls Swarm's rolling-update policy for a service.
type Update struct {
	Parallelism int    `json:"parallelism,omitempty"` // default 1
	Delay       string `json:"delay,omitempty"`       // default "10s"
	Order       string `json:"order,omitempty"`       // "start-first" (default) | "stop-first"
}
