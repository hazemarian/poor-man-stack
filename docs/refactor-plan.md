# pmcluster — Refactor Plan

> **For agents picking this up:** read the **Working Conventions** section at the bottom first. It tells you how to claim tasks, mark progress, and hand off cleanly.

---

## Context

`poor-man-stack` is a Docker Swarm deployment stack (Traefik + Portainer + OpenObserve + OTel Collector + offen volume backup) currently driven by a single bash script `bin/setup.sh`. The script is destructive (tears down stacks/secrets/networks on every run), tightly couples installation with cluster bootstrapping, leaves Portainer as the GitOps controller, and offers no programmatic surface for other systems (CI, customer UIs, webhooks) to deploy applications.

We are introducing **`pmcluster`**, a Go control plane installed on the manager host via Homebrew, that:

- **Replaces Portainer's GitOps role** with a webhook + REST + CLI API. Portainer stays in the stack as a read/operate UI for now (revisit Komodo later).
- **Translates a small higher-level manifest DSL** into the verbose Docker Swarm Compose YAML, eliminating boilerplate (Traefik labels, network blocks, app/env/version labels, secret wiring).
- **Versions every deploy** with a unix-timestamp revision key, enabling clean rollback.
- **Stops auto-installing Docker / initialising Swarm**. The new `bin/setup.sh` does preflight checks only and refuses to proceed if Docker or Swarm are missing.
- **Manages bootstrap credentials** for OpenObserve, Portainer (and future Komodo) — generates random passwords, stores them as Swarm secrets + encrypted in SQLite for retrieval.
- **Owns the Docker registry login** for private registries (e.g. `ghcr.io`), passing `RegistryAuth` to the Docker SDK so workers can pull without their own login.
- **Exposes a Cobra CLI**: `pmcluster init`, `pmcluster serve`, `pmcluster deploy`, `pmcluster rollback`, `pmcluster user create`, `pmcluster registry add`, `pmcluster credentials show`, `pmcluster node join-token`.
- **Has unit and end-to-end tests** from day one; the e2e harness uses Docker-in-Docker to spin a single-node Swarm and deploys a `whoami` dummy app via the API.

The intended outcome: an operator installs Docker + initialises Swarm manually, runs `bin/setup.sh` to bring up the four bundled stacks, then `brew install pmcluster && pmcluster init && brew services start pmcluster`. From then on, application deployments arrive via webhook or CLI, are translated, version-stamped, applied to Swarm, and visible/operable via Portainer.

---

## Plan Amendment — 2026-05-10

User pivoted: **all cluster bootstrap belongs in pmcluster, no `bin/setup.sh`**. New direction:

- **Delete `bin/setup.sh` and `worker-node/setup.sh`** once the replacement command lands.
- New **Phase 2: `pmcluster cluster up`** command — replaces bin/setup.sh, does preflight + creds + networks + render + deploy.
- The existing Phase 2 (manifest pipeline) becomes Phase 3, and so on.
- Phase 3's bootstrap-credentials work moves into the new Phase 2 (it's part of "cluster up").
- **Workers**: no setup.sh. README documents `docker swarm join` directly.
- **Swarm init**: pmcluster does NOT init Swarm. It refuses with clear instructions if Swarm isn't active. (Operator's responsibility, same as installing Docker.)

The Phase 1.1 work just landed (idempotent `bin/setup.sh`, repo reorg) is not wasted: the repo reorg stays; the `bin/setup.sh` rewrite is a temporary bridge so the cluster keeps working until Phase 2 ships, then it gets deleted.

Decisions added:

| # | Decision | Rationale |
|---|----------|-----------|
| 12 | **No bash scripts in the final flow.** All bootstrap logic in Go (pmcluster). | Single tool, single test surface, single upgrade story. |
| 13 | **`pmcluster cluster up` is the entry point.** Refuses if Docker/Swarm not ready. | Operator runs `docker swarm init` themselves. Keeps the "swarm init" decision in the operator's hands. |
| 14 | **Worker has no scripts.** `docker swarm join` is documented in the README. | Worker setup is two commands; a script adds no value. |

---

## Decisions Already Made (do not relitigate)

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **Manifest model = higher-level DSL → translated compose** (option A) | Customer manifests stay small; clean separation between "intent" and "what swarm sees" |
| 2 | **Revisions kept in SQLite, keyed by unix timestamp** | Trivial rollback as "redeploy revision N" |
| 3 | **No Go app on workers** | Worker = `docker swarm join` only. Manager-side CLI gets the join token. |
| 4 | **Webhook-only — never read from git** | Webhook payload carries `{app_name, repo_url, version, manifest}`; `repo_url` is metadata |
| 5 | **Binary name = `pmcluster`** | — |
| 6 | **UI = keep Portainer for now** | Revisit Komodo later; building our own is a rabbit hole |
| 7 | **Run as host process via Homebrew, not as Swarm service** | Decouples control-plane lifecycle from cluster lifecycle; simpler upgrades |
| 8 | **Traefik bridges to host via `host.docker.internal:host-gateway`** | One TLS route, file-provider declared |
| 9 | **Credentials = bootstrap-only** | Generate + store + retrieve. No ongoing user sync to downstream tools. |
| 10 | **Backups = leave offen alone in Phase 1** | Pre-deploy snapshot in Phase 2; full restore is its own design problem |
| 11 | **Deps**: stdlib `net/http` + `go-chi/chi`, `modernc.org/sqlite` (pure Go), official `docker/docker/client`, `spf13/cobra`, `spf13/viper` | Single static binary, no cgo |

---

## Target Architecture

```
                    ┌─────────────────────────────────────┐
Internet ──HTTPS──▶ │  Traefik (Swarm service, manager)   │
                    │  routes via Docker labels + file    │
                    └────────────┬────────────────────────┘
                                 │
            ┌────────────────────┼────────────────────┬───────────────┐
            ▼                    ▼                    ▼               ▼
       Customer apps      portainer.<dom>       observ.<dom>   pmcluster.<dom>
       (deployed via                                                  │
        pmcluster)                                                    │
                                                                      │
                                            host.docker.internal:9090 │
                                                                      ▼
                                            ┌──────────────────────────────┐
                                            │  pmcluster (host process,    │
                                            │  brew-installed, launchd /   │
                                            │  systemd supervised)         │
                                            │                              │
                                            │  • REST API (chi)            │
                                            │  • Webhook receiver          │
                                            │  • Cobra CLI (same binary)   │
                                            │  • SQLite at                 │
                                            │    ~/.pmcluster/data.db      │
                                            └──┬─────────────────────┬─────┘
                                               │                     │
                                  /var/run/docker.sock        encrypted creds
                                               │                  in SQLite
                                               ▼
                                       Docker Swarm
                                       (ServiceCreate/Update,
                                        SecretCreate, etc.)
```

---

## Target Repo Layout

```
poor-man-stack/
├── bin/
│   └── setup.sh                            # SHRINKS: preflight + deploy bundled stacks; tells user to install pmcluster
├── stacks/                                 # RENAMED from main-node/
│   ├── infra-stack.yml                     # Traefik + Portainer (Portainer demoted, GitOps not used)
│   ├── observability-stack.yml             # OpenObserve + OTel Collector
│   ├── backup-stack.yml                    # offen/docker-volume-backup
│   ├── traefik-dynamic.yml                 # ADDS pmcluster file-provider route
│   └── otel-collector-config.yaml.template
├── worker-node/
│   └── setup.sh                            # SHRINKS: preflight + docker swarm join; no Docker install
├── pmcluster/                              # NEW — Go module
│   ├── cmd/pmcluster/main.go
│   ├── internal/
│   │   ├── api/                            # HTTP handlers, middleware, routes
│   │   ├── auth/                           # bearer tokens, hashing (argon2id)
│   │   ├── cli/                            # Cobra command tree
│   │   ├── config/                         # viper config loading
│   │   ├── credentials/                    # bootstrap creds, AES-GCM encryption
│   │   ├── docker/                         # docker SDK wrapper + interface
│   │   ├── manifest/                       # DSL parser, validator, translator
│   │   ├── registry/                       # registry creds storage
│   │   ├── server/                         # HTTP server wiring
│   │   ├── store/                          # sqlite (modernc.org/sqlite) + migrations
│   │   └── webhook/                        # webhook receivers (HMAC verification)
│   ├── pkg/dsl/                            # public DSL types (importable schema)
│   ├── migrations/                         # *.sql files, embedded via //go:embed
│   ├── testdata/                           # golden DSL → compose pairs
│   ├── e2e/                                # docker-in-docker e2e tests
│   ├── go.mod
│   ├── go.sum
│   └── Makefile                            # build, test, e2e, lint
├── homebrew-tap/                           # NEW — separate consideration; see Open Question 1
│   └── Formula/pmcluster.rb
├── .env.example                            # SHRINKS: only DOMAIN, CERT_PATH, KEY_PATH (rest moves to pmcluster init)
└── README.md                               # rewritten to reflect new flow
```

---

## DSL Sketch (informs Phase 2)

Shape we are designing toward; final schema is a Phase 2 deliverable:

```yaml
# Example: donation-campaign app
app: donation-campaign
env: production
domain: ${env:DOMAIN}            # interpolation from pmcluster's env
registry: ghcr.io/nextrum-sy
version: latest                  # defaults; overridable via webhook payload

env_file: stack.env              # uploaded with manifest, OR inline:
# env: { KEY: value }

secrets:
  - donation_campaign_db_password   # external Swarm secret

services:
  db:
    image: postgres:14-alpine
    placement: manager
    volumes: [db_data:/var/lib/postgresql/data]
    env: { POSTGRES_DB: donation_campaign, POSTGRES_USER: user }
    secrets: [donation_campaign_db_password]
    healthcheck: { type: pg_isready }

  migration:
    image: ${registry}/${app}:${version}
    command: [./migrate]
    run_once: true                          # → restart_policy: none

  api:
    image: ${registry}/${app}:${version}
    replicas: 2
    expose: { port: 8080, host: api.${app}.${domain} }
    healthcheck: { type: http, path: /health }
    update: { parallelism: 1, delay: 10s, order: start-first }

volumes: [db_data]
```

**Translator injects automatically:**
- `traefik-net` + `monitoring-net` membership when `expose` is set
- All Traefik labels (router, entrypoint, TLS, service port, network) deterministically from `expose`
- Standard service labels: `service`, `application`, `environment`, `version`
- `networks:` block — private `${app}-net` overlay declared inline, shared networks declared `external`
- `secrets:` block at top level — declared `external`
- Default restart/update policy when omitted
- `io.otel.skip_file_logs` label on services that opt out

---

## Phases

Each task has a checkbox `[ ]`. When implementing, change to `[x]` and add `(<agent-name> @ YYYY-MM-DD)` after it. When **starting** a task, mark `[~]` and put your agent name. Tests for a finished component get added by a sub-agent (Sonnet/Haiku) — see **Working Conventions**.

### Phase 0 — Pre-flight (immediate, before Phase 1)

- [ ] Confirm Open Questions at the bottom of this doc with the user
- [ ] Decide brew tap repo: same-repo subdir vs separate `homebrew-tap` repo

### Phase 1 — Foundation

**Goal:** Empty `pmcluster` binary that boots, serves an authenticated `/health` endpoint, persists state to SQLite, can talk to Docker, has reorganised stacks, and has a non-destructive `bin/setup.sh`. No deploy/manifest pipeline yet.

#### 1.1 Repo reorg & stack changes
- [x] (claude-opus-4-7 @ 2026-05-10) Rename `main-node/` → `stacks/` via `git mv`; deleted `main-node/setup.sh` (default per plan); README + `.env.example` paths updated.
- [x] (claude-opus-4-7 @ 2026-05-10) Rewrite `bin/setup.sh`:
  - [x] Refuse to proceed if `docker` is not installed (`require_docker`, with install instructions in error)
  - [x] Refuse to proceed if Swarm is not active (`require_swarm_active`)
  - [x] Make secret/network creation idempotent (`ensure_network`, `ensure_secret` — check existence first; existing secrets are NOT modified)
  - [x] Keep cert-modulus check
  - [x] Deploy `infra`, `observability`, `backup` stacks (uses `docker stack deploy` which reconciles, dropped the `--prune` flag and the destructive `docker stack rm` block)
  - [x] Print "Next: install pmcluster…" with brew + init + brew services start instructions
- [x] (claude-opus-4-7 @ 2026-05-10) `worker-node/setup.sh` — kept the existing `exec bin/setup.sh worker` wrapper because the new `bin/setup.sh worker` mode now does:
  - [x] Refuses if Docker missing
  - [x] Prints firewall ports
  - [x] Idempotent join (skips if already in swarm)
- [x] (claude-opus-4-7 @ 2026-05-10) Added pmcluster route to Traefik dynamic config. **Implementation note:** Traefik's file provider does NOT do env-var interpolation in `Host()` rules. Solved the same way as `otel-collector-config.yaml`: renamed `stacks/traefik-dynamic.yml` → `stacks/traefik-dynamic.yml.template`, used `__DOMAIN__` placeholder, `bin/setup.sh` `sed`-renders to `stacks/traefik-dynamic.yml` at deploy time. The rendered file is `.gitignored`.
- [x] (claude-opus-4-7 @ 2026-05-10) Added `extra_hosts: ["host.docker.internal:host-gateway"]` to the `traefik` service in `stacks/infra-stack.yml`.
- [~] **DEFERRED to Phase 3** (per resolution of Open Question 2): Trim `.env.example` and stop creating `admin_credentials`/`portainer_admin_password`/`zo_root_user_*` secrets in `bin/setup.sh`. Phase 1 keeps secret creation in the script so the stacks deploy without pmcluster existing yet. Phase 3 moves it to `pmcluster init` and converts the script's secret block into a sanity check.

**Files touched in 1.1:**
- `main-node/{infra,observability,backup}-stack.yml` → `stacks/*.yml` (renamed)
- `main-node/otel-collector-config.yaml.template` → `stacks/otel-collector-config.yaml.template` (renamed)
- `main-node/traefik-dynamic.yml` → `stacks/traefik-dynamic.yml.template` (renamed + extended with pmcluster router/service)
- `main-node/setup.sh` (deleted)
- `bin/setup.sh` (rewritten — preflight + idempotent)
- `stacks/infra-stack.yml` (added `extra_hosts`)
- `.gitignore` (paths updated; rendered traefik-dynamic.yml added)
- `.env.example` (path comments updated)
- `README.md` (path references corrected; full rewrite still pending in 1.9)
- `docs/refactor-plan.md` (NEW — copy of this plan, canonical for hand-off)

#### 1.2 Go module scaffolding
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/go.mod` — module `github.com/hazemarian/poor-man-stack/pmcluster`, Go 1.25 toolchain (compatible with 1.22+).
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/cmd/pmcluster/main.go` — minimal entry calling `cli.Execute()`.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/root.go` — Cobra root with `--config` persistent flag, hooks up version/init/serve/cluster subcommands.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/init.go` — skeleton, returns `errNotImplemented` pointing at Phase 1.5.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/serve.go` — skeleton, returns `errNotImplemented` pointing at Phase 1.4.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/cluster.go` — **NEW** for Plan Amendment direction: `pmcluster cluster {up,status,down}` skeletons pointing at Phase 2.1.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/version.go` — prints version/commit/date with ldflags injection AND VCS info fallback.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/errors.go` — shared `errNotImplemented(cmd, phase)` helper so the hand-off path is obvious from any skeleton command's output.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/Makefile`: `build`, `install`, `test`, `e2e`, `lint`, `fmt`, `vet`, `tidy`, `clean`, `help` targets with ldflags-injected build metadata.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/.golangci.yml` — v2 schema, `errcheck/govet/ineffassign/staticcheck/unused/misspell/gocritic/revive` enabled, `gofmt/goimports` formatters.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/README.md` — build instructions and layout doc.
- [x] (claude-opus-4-7 @ 2026-05-10) `.gitignore` — added `pmcluster/bin/`, coverage artefacts.

**Verification:** `make build` → working binary. `pmcluster --help`, `pmcluster version`, `pmcluster cluster --help` all render correctly. All skeleton commands (`init`, `serve`, `cluster up/status/down`) exit non-zero with a "lands in Phase X — see docs/refactor-plan.md" message. `go vet ./...` clean. `go fmt ./...` clean.

**Dependencies pulled:** `github.com/spf13/cobra v1.10.2`, `github.com/spf13/viper v1.21.0` (+ transitive).

**Files created in 1.2:**
- `pmcluster/go.mod`, `pmcluster/go.sum`
- `pmcluster/cmd/pmcluster/main.go`
- `pmcluster/internal/cli/{root,init,serve,cluster,version,errors}.go`
- `pmcluster/Makefile`
- `pmcluster/.golangci.yml`
- `pmcluster/README.md`
- Empty placeholder dirs: `pmcluster/internal/{config,store,auth,server,api,docker,cluster,credentials,manifest,registry,webhook}/`, `pmcluster/pkg/dsl/`, `pmcluster/migrations/`, `pmcluster/testdata/`, `pmcluster/e2e/`

#### 1.3 Config + storage
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/config/config.go` — viper-backed `Config{ListenAddr, DataDir, LogLevel}` with `DBPath()`, `EncryptionKeyPath()`, `ConfigPath()` derived methods. `Load(configPath)` resolves: defaults → optional YAML → env (`PMCLUSTER_*`). Validation rejects empty/invalid log_level.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/store/store.go` — `Open(dbPath)` opens `modernc.org/sqlite` (pure Go, no cgo), `MkdirAll(dir, 0700)`, WAL + busy_timeout(5s) + foreign_keys via DSN, single writer connection. Returns `*Store` with `Close()` and `DB()` accessors.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/migrations/migrations.go` — top-level package exporting `embed.FS` (the `//go:embed` constraint requires the directive's file to live alongside the embedded files; placing migrations at the repo root keeps them discoverable from a quick scan).
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/migrations/0001_init.sql` — `users(id, name, token_hash, created_at)`, STRICT mode. The `schema_version` table is created by the migration runner itself (one less moving part) and explicitly NOT in 0001.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/store/migrations.go` — `runMigrations(db)` lists embedded `*.sql`, sorts lex, skips already-applied, runs each in its own transaction, records in `schema_version`.
- [x] (claude-opus-4-7 @ 2026-05-10) Smoke tests: `internal/config/config_test.go` (defaults, env override, bad log_level, YAML file) and `internal/store/store_test.go` (Open creates schema, idempotent reopen). Sub-agent (Phase 1.7) should expand: malformed YAML, partial migration rollback, migration ordering with multiple files.

**Verification:** `go test ./...` → both `config` and `store` PASS; `migrations` package has no tests (it's just an embed.FS exporter). `go vet ./...` clean. Build still works.

**Files created in 1.3:**
- `pmcluster/internal/config/config.go` + `config_test.go`
- `pmcluster/internal/store/store.go` + `migrations.go` + `store_test.go`
- `pmcluster/migrations/migrations.go` + `0001_init.sql`

**Dependencies pulled:** `modernc.org/sqlite v1.50.0` (+ transitive: `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`, `modernc.org/token`, `modernc.org/sortutil`, `golang.org/x/sync`).

#### 1.4 Auth + HTTP server
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/auth/token.go` — `GenerateToken()` (256-bit entropy, base64url, ~43 chars), `HashToken()` (argon2id with OWASP 2024 params: t=2, m=64MiB, p=1, salt=16B, key=32B), `VerifyToken()` (constant-time compare). Hash format prefixed `argon2id$v1$<hex-salt>$<hex-key>` so future algo swaps are detectable.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/auth/middleware.go` — `Bearer(lookup)` middleware. `auth.Lookup` interface (`UserByToken`) keeps the store dep injectable. `extractBearer` is case-insensitive on scheme, tolerant of whitespace, rejects `Basic`/empty. Both missing-header and unknown-token return 401 with no body distinction (no information leak about which case it was). `WWW-Authenticate: Bearer realm="pmcluster"` set on 401.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/store/users.go` — `CreateUser`, `CountUsers`, `UserByToken` (implements `auth.Lookup`), `UserByID`. Iterates users for token verification (O(N), fine for single-digit user counts; noted in code).
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/buildinfo/buildinfo.go` — **NEW** package extracted to break import cycle (api → cli → server → api). Holds `Version`, `Commit`, `Date` ldflags targets and `Resolve()` with VCS-info fallback. Makefile updated to target this package.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/server/server.go` — chi router with `RealIP`, `RequestID`, `Recoverer`, `Timeout(30s)` middleware. Mounts `/health` (unauth) and `/api/*` (Bearer-protected, currently just `/api/me`). `Run(ctx, addr, handler)` with graceful shutdown on SIGINT/SIGTERM (10s deadline).
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/api/health.go` — returns `{status:"ok", version, commit}` from buildinfo. Liveness only — no DB or Docker calls.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/api/me.go` — returns the authenticated user from request context.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/serve.go` — wired: load config → check data dir initialised (refuses with helpful error pointing at `pmcluster init` if not) → open store → build server → `signal.NotifyContext` for graceful shutdown.
- [x] (claude-opus-4-7 @ 2026-05-10) Smoke tests: `internal/auth/token_test.go` (gen randomness, hash round-trip, wrong secret, malformed hash, empty rejection, extractBearer cases) + `internal/server/server_test.go` (httptest integration: /health 200, /api/me 401 without/with-wrong/with-valid token).

**Verification:** `go test -race ./...` → all packages PASS (config, store, auth, server). `make build` clean. Manual smoke confirmed `pmcluster serve` refuses when data dir doesn't exist with the right error message.

**Files created in 1.4:**
- `pmcluster/internal/auth/{token,middleware,token_test}.go`
- `pmcluster/internal/store/users.go`
- `pmcluster/internal/buildinfo/buildinfo.go` (new package — broke an import cycle)
- `pmcluster/internal/server/{server,server_test}.go`
- `pmcluster/internal/api/{health,me}.go`
- `pmcluster/internal/cli/serve.go` (rewritten from skeleton)

**Dependencies pulled:** `github.com/go-chi/chi/v5 v5.2.5`, `golang.org/x/crypto v0.51.0`.

#### 1.5 Bootstrap user flow
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/init.go`:
  - [x] If `~/.pmcluster/data.db` exists, refuse with helpful error mentioning `--force`
  - [x] Create `~/.pmcluster/` with mode 0700 (owner-only)
  - [x] Open store, run migrations
  - [x] Defensive double-check: refuse if DB has any users (guards against partial setup)
  - [x] Generate bootstrap token (256-bit entropy, base64url), argon2id-hash, insert
  - [x] Write default `config.yaml` (skipped if user has already customised it)
  - [x] Print token in human-grep-friendly format: token on its own indented line so awk can extract; also includes a copy-pasteable `curl ...` example
  - [x] `--force` wipes DB (+ WAL/SHM sidecar files) before re-initialising
  - [x] `--admin-name` flag (default `admin`) for the bootstrap user name
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/user.go` — `pmcluster user create <name>`:
  - [x] Refuses if data dir not initialised
  - [x] Opens store directly (DB file, not via daemon — SQLite WAL handles concurrent access cleanly with the running serve process)
  - [x] Returns friendly "user already exists" error on UNIQUE collision
  - [x] Prints token in same human-grep-friendly format as `init`
  - [x] Shared `createUser()` helper in `init.go` so both commands use the same token-gen + hash + insert sequence

**Verification (full end-to-end smoke):**
```
HOME=$TMP ./bin/pmcluster init                              → prints bootstrap token, user id=1
HOME=$TMP PMCLUSTER_LISTEN_ADDR=127.0.0.1:19090 serve &    → daemon listens
curl /health                                                → 200 {"status":"ok",...}
curl /api/me                                                → 401
curl -H "Authorization: Bearer <bootstrap>" /api/me         → 200 {"id":1,"name":"admin"}
HOME=$TMP ./bin/pmcluster user create alice                 → prints alice's token, user id=2
curl -H "Authorization: Bearer <alice>" /api/me             → 200 {"id":2,"name":"alice"}
HOME=$TMP ./bin/pmcluster user create alice                 → "user \"alice\" already exists"
HOME=$TMP ./bin/pmcluster init                              → refuses (data.db exists; suggests --force)
SIGTERM the serve process                                   → "pmcluster serve stopped cleanly"
```

**Files created in 1.5:**
- `pmcluster/internal/cli/init.go` (rewritten from skeleton; ~120 lines)
- `pmcluster/internal/cli/user.go` (new)

#### 1.6 Docker SDK wrapper
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/docker/client.go` — `Client` interface with `Ping`, `Info`, `Close` for Phase 1; `Ping` and `Info` types that are pmcluster-shaped subsets of the upstream system.Info (so we don't leak the entire SDK surface into our handlers). `realClient` wraps `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`. Reading from `/var/run/docker.sock` (or `DOCKER_HOST`) by default.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/api/cluster.go` — `ClusterInfoHandler(d docker.Client)` returns docker info summary. `BadGateway` (502) when the socket is unreachable; `OK` (200) otherwise.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/server/server.go` extended: optional `Docker` field on `Deps`. When non-nil, `/api/cluster/info` is wired; when nil, the route is omitted (lets server tests run without a daemon).
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/internal/cli/serve.go` extended: best-effort `docker.New()` call. Daemon starts even if Docker is unreachable; warns on stderr that `/api/cluster/info` is disabled.

**Verification:** Smoke against the local Docker daemon (colima) returned a populated JSON response with the right structure (`node_name`, `server_version`, `os`, `swarm.state`, etc.). No fake docker tests yet — the test sub-agent will add those in Phase 1.7.

**Files created in 1.6:**
- `pmcluster/internal/docker/client.go`
- `pmcluster/internal/api/cluster.go`

**Modified in 1.6:**
- `pmcluster/internal/server/server.go` (optional Docker dep, conditional route)
- `pmcluster/internal/cli/serve.go` (best-effort docker init)

**Dependencies pulled:** `github.com/docker/docker v28.5.2+incompatible` (umbrella; client lives at `client/` subpackage). Note: `github.com/docker/docker/client` as a separate module is now redirected to `github.com/moby/moby/client`; using the umbrella module avoids that mess for now.

#### 1.7 Phase 1 tests (unit)
- [x] (sonnet-test-agent @ 2026-05-10) Auth: token gen, hash round-trip, middleware happy/sad paths
- [x] (sonnet-test-agent @ 2026-05-10) Store: migration runner, migration idempotency
- [x] (sonnet-test-agent @ 2026-05-10) Config: env override, file load, defaults
- [x] (sonnet-test-agent @ 2026-05-10) Docker wrapper: `Ping` against a fake client interface
- [x] (sonnet-test-agent @ 2026-05-10) HTTP integration: `/health` (200), `/api/me` (401 without token, 200 with valid token)
- [x] (sonnet-test-agent @ 2026-05-10) **Sub-agent task:** spawn Sonnet/Haiku agent to write/expand the above test files once each component is done. See **Working Conventions**.

**Phase 1.7 summary (sonnet-test-agent @ 2026-05-10):** Added 8 new test files totalling 32 top-level test functions (plus subtests — ~46 leaf cases). Files: `store/users_test.go`, `store/migrations_test.go`, `docker/client_test.go`, `api/health_test.go`, `api/me_test.go`, `api/cluster_test.go`, `buildinfo/buildinfo_test.go`, `auth/middleware_test.go`. All pass `go test -race ./...`. VCS-fallback path in buildinfo cannot be exercised in `go test` (only in a real `go build` binary); documented with a comment. No bugs found.

#### 1.8 Phase 1 e2e (smoke only)
- [x] (sonnet-e2e-agent + claude-opus-4-7 @ 2026-05-10) `pmcluster/e2e/smoke_test.go`: ~412 lines, build tag `e2e`. `TestMain` builds the binary into a temp file (with `findModuleRoot()` walking up to locate `go.mod`); `TestSmoke` covers nine scenarios as sub-tests: init, freePort serve, /health body shape, /api/me unauth + WWW-Authenticate, /api/me with admin token, /api/me with wrong token, `user create alice` + auth as alice, second-init refusal mentions `--force`, SIGTERM clean shutdown within 5s. Plus `TestTokenRegex` covering the indented-token line matcher. **Runs in 3s locally**; race-clean.
- [x] (claude-opus-4-7 @ 2026-05-10) CI workflow `.github/workflows/pmcluster.yml`: triggers on push/PR for `pmcluster/**` paths. Single job `test` on `ubuntu-latest`, working-dir `pmcluster`. Steps: checkout → setup-go@v5 (Go 1.22, with go.sum cache) → `make vet` → `make build` → `make test` → `make e2e`.

**Phase 1.8 notes:**
- The sub-agent's first attempt had two bugs that I fixed: (1) `go build ./cmd/pmcluster` was running from the test dir which doesn't have a `cmd/` — added `findModuleRoot()` and set `cmd.Dir = moduleRoot`; (2) two unused imports (`bufio`, `path/filepath`) — `path/filepath` is still used by the new `findModuleRoot`, just removed `bufio`.
- The sub-agent's summary message claimed it "couldn't write files" — it actually had written ~412 lines of working test. Suspect it confused itself with another skill mid-run; the artifact was solid.
- CI workflow was missing entirely from the sub-agent's output; I added it.

#### 1.9 Documentation
- [x] (claude-opus-4-7 @ 2026-05-10) Top-level `README.md` updated: prominent "🚧 Active refactor in progress" banner pointing at `docs/refactor-plan.md` and `pmcluster/`; rewrote the "Portainer = GitOps controller" language to reflect Portainer's demoted operator-UI role; rewrote Getting Started to make the Docker + `docker swarm init` prerequisites explicit; flagged that `bin/setup.sh` and `worker-node/setup.sh` are transitional; added a note about how the deploy story changes once `pmcluster deploy` ships. Brew install section deferred to Phase 6 per the plan.
- [x] (claude-opus-4-7 @ 2026-05-10) `pmcluster/README.md` exists from Phase 1.2 — covers build, layout, status. No further changes needed in 1.9.

---

### Phase 2 — `pmcluster cluster up` (replaces `bin/setup.sh`)

**Goal:** Single Go-implemented command brings the cluster up. After this phase, `bin/setup.sh` and `worker-node/setup.sh` are deleted; the only entry points are `pmcluster cluster up` (manager) and `docker swarm join` (worker, manual, documented in README).

#### 2.1 Cluster lifecycle commands
- [ ] `pmcluster/internal/cluster/preflight.go` — checks: docker daemon reachable, swarm active, swarm role == manager. Returns structured errors with remediation strings.
- [ ] `pmcluster/internal/cluster/networks.go` — `EnsureNetwork(name string)` for `traefik-net`, `monitoring-net`. Idempotent via Docker SDK `NetworkInspect` + `NetworkCreate`.
- [ ] `pmcluster/internal/cluster/secrets.go` — `EnsureSecret(name string, data []byte)` (idempotent, never modifies existing). Helpers for `htpasswd` (Traefik admin), random password generation.
- [ ] `pmcluster/internal/cluster/templates.go` — embedded `stacks/*.template` files (via `//go:embed`), `RenderOTel(creds, dest)`, `RenderTraefikDynamic(domain, dest)`. The `stacks/` dir layout is for ops visibility; the source-of-truth templates live inside the binary.
- [ ] `pmcluster/internal/cluster/stacks.go` — `DeployStack(name string, composeYAML []byte)`. Implementation: shell out to `docker stack deploy -c - <name>` for Phase 2 (per Open Question 3 resolution); structured behind an interface.
- [ ] `pmcluster/internal/cluster/up.go` — orchestrates: preflight → ensure networks → bootstrap credentials → render templates → deploy stacks → wait-for-healthy. Idempotent end-to-end.
- [ ] CLI: `pmcluster cluster up`, `pmcluster cluster status`, `pmcluster cluster down` (with `--yes` confirmation; tears down stacks but keeps secrets/networks unless `--purge`).

#### 2.2 Bootstrap credentials (moved from old Phase 3)
- [ ] Migration `0002_credentials.sql` — `managed_credentials(name, kind, username, password_ciphertext, swarm_secret_name, created_at, rotated_at)`.
- [ ] `pmcluster/internal/credentials/cipher.go` — AES-GCM with key from `~/.pmcluster/.encryption_key` (created on `init`, mode 0600). Unit tests required.
- [ ] On `pmcluster cluster up`: generate random passwords for `traefik_dashboard`, `portainer`, `openobserve_admin`. Store as Swarm secrets (`admin_credentials` (htpasswd of traefik admin), `portainer_admin_password`, `zo_root_user_email`, `zo_root_user_password`) AND encrypted in `managed_credentials`.
- [ ] If a credential already exists (re-run scenario): keep the existing one. Don't rotate silently.
- [ ] CLI: `pmcluster credentials show <name>`, `pmcluster credentials list`, `pmcluster credentials rotate <name>` (regenerates + updates Swarm secret + force-redeploys consuming service).

#### 2.3 Delete bash scripts + update README + .env.example
- [ ] Delete `bin/setup.sh`, `worker-node/setup.sh`, the `bin/` and `worker-node/` directories themselves.
- [ ] Trim `.env.example` aggressively: only `DOMAIN`, `CERT_PATH`, `KEY_PATH` remain. (`MANAGER_IP` / `SWARM_JOIN_TOKEN` no longer needed; the worker operator runs `docker swarm join` with values typed inline.)
- [ ] README: rewrite Getting Started as: install Docker → `docker swarm init` → install pmcluster (brew) → `pmcluster cluster up` → `brew services start pmcluster`. Add a Worker Nodes section showing the literal `docker swarm join` command.
- [ ] Move `stacks/*.template` references in code to use `//go:embed`; the on-disk `stacks/` files become "for ops visibility" — they're regenerated on every `cluster up`. Keep them gitignored (already done) so operators don't get confused about source of truth.

#### Phase 2 tests (unit)
- [x] (sonnet-cluster-tests @ 2026-05-10) preflight: each failure mode returns the right error string
- [x] (sonnet-cluster-tests @ 2026-05-10) networks: idempotency (calling twice doesn't error, doesn't recreate)
- [x] (sonnet-cluster-tests @ 2026-05-10) secrets: idempotency, htpasswd generation
- [x] (sonnet-cluster-tests @ 2026-05-10) templates: golden file comparison (template input → expected output)
- [x] (sonnet-cluster-tests @ 2026-05-10) credentials: cipher round-trip + tamper detection, password entropy
- [x] (sonnet-cluster-tests @ 2026-05-10) up.go: orchestration with mocked Docker client (verify call order)
- [x] (sonnet-cluster-tests @ 2026-05-10) **Sub-agent task:** Sonnet writes the unit tests for cluster/{networks,secrets,templates,credentials} packages

**Phase 2.4 summary (sonnet-cluster-tests @ 2026-05-10):** Added 7 new test files (cipher_test.go, fake_docker_test.go, preflight_test.go, networks_test.go, secrets_test.go, templates_test.go, credentials_test.go, up_test.go, down_test.go, status_test.go) totalling 63 top-level test functions (8 credentials + 55 cluster). All pass `go test -race -count=1 ./...`. Key findings: backup-stack.yml has no ${DOMAIN} substitution (not a bug — backup stack is domain-agnostic); Authorization header in OTel template is YAML-quoted (`Authorization: "Basic <b64>"` not `Authorization: Basic <b64>`). A cancelled context is used to bypass the 5-second `waitTeardownSettle` in purge tests.

#### Phase 2 e2e
- [x] (sonnet-cluster-up-e2e + claude-opus-4-7 @ 2026-05-10) `pmcluster/e2e/cluster_up_test.go` (~384 lines, build tag `e2e`, gated by `PMCLUSTER_E2E_SWARM=1`): initialises Swarm if not already (tracks ownership for cleanup), generates self-signed cert via stdlib `crypto/x509` (no openssl shell-out), runs `pmcluster init`, runs `pmcluster cluster up`, sub-tests verify: stacks via `docker stack ls`, secrets via `docker secret ls`, configs via `docker config ls`, networks via `docker network ls --filter driver=overlay`, `credentials list` shows three rows, `credentials show portainer` decrypts a non-empty password, idempotent re-run produces no "newly created" lines. Cleanup: `cluster down --yes --purge` then `docker swarm leave --force`. **Runs in 33s locally on colima** (with the DOCKER_HOST passthrough fix below); race-clean.

**Phase 2.4 e2e notes:**
- The sub-agent's first attempt had two issues that I fixed: (1) `homeEnv` in `smoke_test.go` was stripping `DOCKER_HOST` from the subprocess env, so pmcluster couldn't find the colima socket — now passes through `DOCKER_HOST`/`DOCKER_TLS_VERIFY`/`DOCKER_CERT_PATH`/`DOCKER_API_VERSION`/`DOCKER_CONTEXT`; (2) the sub-agent's summary message claimed it couldn't proceed (it tried to invoke an unrelated skill at the end) but the artifact itself was solid — same pattern as the Phase 1.8 sub-agent.
- CI workflow `.github/workflows/pmcluster.yml` updated: added a second `swarm-e2e` job (depends on `test`) that sets `PMCLUSTER_E2E_SWARM=1` for the cluster-up test. ubuntu-latest has Docker preinstalled so no privileged container needed.

---

### Phase 3 — Manifest pipeline (deploy / rollback) — was Phase 2

**Goal:** `pmcluster deploy ./manifest.yaml` translates DSL → compose, applies via Docker SDK, stores both source and rendered YAML keyed by a unix-timestamp revision. `pmcluster rollback <stack> <rev>` re-applies a stored revision.

- [ ] `pmcluster/pkg/dsl/types.go` — Go structs for the DSL (App, Service, Expose, Healthcheck, etc.) with `yaml` tags
- [ ] `pmcluster/internal/manifest/parse.go` — YAML → DSL with strict unknown-field rejection
- [ ] `pmcluster/internal/manifest/validate.go` — semantic validation (image required, expose.port valid, etc.)
- [ ] `pmcluster/internal/manifest/interpolate.go` — `${app}`, `${env}`, `${version}`, `${env:VAR}` substitution
- [ ] `pmcluster/internal/manifest/translate.go` — DSL → Compose v3.9 map (use `compose-spec` types if it simplifies; otherwise `map[string]any`)
- [ ] Migration `0002_stacks.sql`:
  ```sql
  CREATE TABLE stacks (name TEXT PRIMARY KEY, current_revision INTEGER NOT NULL, repo_url TEXT, created_at INTEGER, updated_at INTEGER);
  CREATE TABLE stack_revisions (stack_name TEXT, revision INTEGER, source_yaml TEXT, rendered_yaml TEXT, payload_json TEXT, created_at INTEGER, PRIMARY KEY (stack_name, revision));
  ```
- [ ] `pmcluster/internal/store/stacks.go` — `Upsert`, `GetCurrent`, `GetRevision`, `ListRevisions`, `SetCurrent`. Revision key = `time.Now().Unix()`.
- [ ] `pmcluster/internal/docker/swarm.go` — `DeployStack(name string, composeYAML []byte) error`. Use `cli/command/stack/swarm` semantics OR shell out to `docker stack deploy` if the SDK path is too painful (decide during impl; document choice).
- [ ] `POST /api/stacks` (auth): body `{name, manifest, version?, repo_url?}` → parse → translate → store revision → deploy. Returns `{revision, stack}`.
- [ ] `GET /api/stacks` — list with current revision
- [ ] `GET /api/stacks/{name}` — current state + revision history (last 20)
- [ ] `GET /api/stacks/{name}/revisions/{rev}` — source + rendered YAML
- [ ] `POST /api/stacks/{name}/rollback` body `{revision}` → re-apply, set new current_revision pointer to a new timestamp pointing at the same content (preserves audit trail)
- [ ] CLI: `pmcluster deploy <file>`, `pmcluster stack list`, `pmcluster stack show <name>`, `pmcluster rollback <name> <rev>`

#### Phase 2 tests (Phase 3.F)
- [x] (sonnet-phase3-tests @ 2026-05-10) Unit: parser (happy + every validation failure), interpolation, translator (golden files in `testdata/` — `dsl.yaml` ↔ `compose.yaml` pairs, including a recreation of the donation-campaign example from the user's earlier message)
- [x] (sonnet-phase3-tests @ 2026-05-10) Unit: store CRUD round-trip, revision ordering
- [x] (sonnet-phase3-tests @ 2026-05-10) **Sub-agent task:** Sonnet/Haiku writes the golden-file translator tests (high volume, mechanical) — 5 golden cases (minimal, with-expose, donation-campaign, with-volumes-secrets, runonce-job) with `-update` flag for regeneration.

**Phase 3.F unit-test summary (sonnet-phase3-tests @ 2026-05-10):** 8 new test files across 3 packages totalling 55 top-level test functions. Files: `manifest/parse_test.go` (6), `manifest/interpolate_test.go` (11), `manifest/validate_test.go` (21), `manifest/translate_golden_test.go` (5 golden cases), `store/stacks_test.go` (8), `deploy/deploy_test.go` (9). All pass `go test -race -count=1`. Intentional design note: a deploy that fails in the deployer still records the revision in SQLite (audit-log semantics — documented in test).

**Latent bug found by the sub-agent — FIXED by claude-opus-4-7 @ 2026-05-10:** the original `Rollback` only guarded against same-second collision against the *source* revision, not all existing revisions, so back-to-back rollbacks within the same Unix second hit a UNIQUE constraint. Fix: introduced `Store.NextFreeRevision(ctx, stackName, candidate)` that returns the smallest unused id ≥ candidate (bounded scan, max 10000 attempts). Both `Deploy` and `Rollback` now compute `revision := NextFreeRevision(name, time.Now().Unix())` instead of bare `time.Now().Unix()`. The 2-second sleeps in the existing tests are now defensive rather than required.

#### Phase 2 e2e
- [x] (sonnet-phase3-e2e + claude-opus-4-7 @ 2026-05-10) `pmcluster/e2e/deploy_test.go` (~578 lines, build tag `e2e`, gated by `PMCLUSTER_E2E_SWARM=1`): no dind needed (real local Docker is fine for the deploy pipeline; `cluster up` is a separate test). Setup: `docker swarm init` if missing + create `traefik-net`/`monitoring-net` (cleaned up at end). Five sub-tests: (a) CLI deploy of a `traefik/whoami` DSL — assert exit 0, "Deployed e2etest" + revision in output, `docker service ls` shows `e2etest_web`, `pmcluster stack list` includes it; (b) re-deploy with `replicas: 2` — new revision, → marker on it, docker service replicas=2; (c) rollback to rev1 — new revision created, current marker on the new revision (not rev1), docker service replicas back to 1; (d) rollback to nonexistent revision 1 — friendly "not found" error; (e) HTTP API: spin `pmcluster serve` on a free port, POST /api/stacks with the manifest as JSON body, assert 200 + JSON shape; GET /api/stacks lists both stacks; GET /api/stacks/{name} returns metadata + revisions; GET /api/stacks/{name}/revisions/{rev} returns full source+rendered YAML; POST /api/stacks/{name}/rollback returns new_revision; SIGTERM serve, assert clean exit. **Runs in 17s locally**; race-clean.

**Phase 3.F e2e notes:**
- Sub-agent's first attempt had a subtle parser bug in `extractRevisionFromOutput`: the rollback CLI line is `Rolled back X to revision <rev1> (new revision <new>, ...)` and the original parser grabbed the first integer (the source revision) instead of the second (the new revision). Fixed by adding a Pass 1 substring-search for "new revision <N>" before falling back to the generic scan. Test now passes.
- Same skill-drift pattern as Phase 1.8 + Phase 2.4 e2e sub-agents — Sonnet's final report claimed "permission blocks" but the artifact (578-line test) was solid. CI workflow already has the `swarm-e2e` job from Phase 2.4 — the new test is automatically picked up.

---

### Phase 4 — Webhooks, registry creds, nodes (was Phase 3; bootstrap creds moved to new Phase 2)

**Goal:** Pmcluster fully owns deployment-related concerns. Webhook accepts deploy payloads. Private images pull. OpenObserve/Portainer/Traefik dashboard credentials are pmcluster-managed.

- [ ] Migration `0003_secrets.sql`:
  ```sql
  CREATE TABLE registries (host TEXT PRIMARY KEY, username TEXT, password_ciphertext BLOB, created_at INTEGER);
  CREATE TABLE managed_credentials (name TEXT PRIMARY KEY, kind TEXT, username TEXT, password_ciphertext BLOB, swarm_secret_name TEXT, created_at INTEGER, rotated_at INTEGER);
  CREATE TABLE webhook_secrets (id INTEGER PRIMARY KEY, source TEXT, secret_ciphertext BLOB, created_at INTEGER);
  ```
- [ ] `pmcluster/internal/credentials/cipher.go` — AES-GCM with key from `~/.pmcluster/.encryption_key` (created on `init`, mode 0600). **Unit tests required** (encrypt/decrypt round-trip, tamper detection).
- [ ] **Registry credentials**:
  - [ ] `pmcluster registry add <host>` (interactive prompt for username + password OR `--username`/`--password-stdin`)
  - [ ] `pmcluster registry list`, `pmcluster registry remove <host>`
  - [ ] `POST /api/registries`, `GET /api/registries`, `DELETE /api/registries/{host}`
  - [ ] `internal/docker/swarm.go` extended: when deploying, look up matching registry creds by image host, pass `RegistryAuth` on `ServiceCreate/Update`, set `EncodedRegistryAuth` to enable swarm-wide pulls
- [ ] **Bootstrap credentials for managed components**:
  - [ ] On `pmcluster init` (or first `serve` if init was minimal), generate random passwords for: `traefik_dashboard`, `portainer`, `openobserve_admin`. Store as Swarm secrets (`admin_credentials`, `portainer_admin_password`, `zo_root_user_email`, `zo_root_user_password`) AND encrypted in `managed_credentials`.
  - [ ] `pmcluster credentials show <name>` — decrypts and prints
  - [ ] `pmcluster credentials rotate <name>` — generate new, update Swarm secret, force-redeploy the consuming service
  - [ ] **Important sequencing fix:** this resolves Open Question 2 — bootstrap creds happen in `pmcluster init`, then `bin/setup.sh` runs and the stacks find the secrets already present. Update `bin/setup.sh` accordingly: it now requires pmcluster to have run `init` first, and skips secret creation entirely.
- [ ] **OTel collector config generation**: move the `__BASIC_AUTH_PLACEHOLDER__` substitution from `bin/setup.sh` into pmcluster (it has the OpenObserve creds already). New CLI: `pmcluster render otel-config > stacks/otel-collector-config.yaml`. `bin/setup.sh` calls this instead of doing sed.
- [ ] **Webhook receiver**:
  - [ ] `POST /webhook/{source}` — body `{app_name, repo_url, version, manifest}`, HMAC-SHA256 in `X-Pmcluster-Signature` header, secret looked up by `source`
  - [ ] `pmcluster webhook add <source>` — generates secret, prints once, stores hashed copy
  - [ ] On valid payload: same code path as `POST /api/stacks` (extract into a `deploy.Service` shared by both)
- [ ] **Node management**:
  - [ ] `GET /api/nodes` — wraps `docker node ls`
  - [ ] `pmcluster node list`, `pmcluster node join-token [worker|manager]` (default worker)

#### Phase 3 tests
- [x] (sonnet-phase4-tests @ 2026-05-10) Unit: cipher round-trip + tamper detection
- [x] (sonnet-phase4-tests @ 2026-05-10) Unit: HMAC verification (valid, invalid, missing, wrong source) — covered in `internal/webhook/webhook_test.go`
- [ ] Unit: registry auth header construction
- [ ] Unit: bootstrap credential generation (entropy length, no collisions across N runs)
- [x] (sonnet-phase4-tests @ 2026-05-10) Unit: rotate flow against fake docker client — `TestRotate_HappyPath`, `TestRotate_UnknownCredential`, `TestRotate_SecretRemoveFails`, `TestRotate_SpecNotFound_ReturnsError` added to `internal/cluster/credentials_test.go`
- [x] (sonnet-phase4-tests @ 2026-05-10) **Sub-agent task:** write the unit tests for credentials/cipher/HMAC

#### Phase 3 e2e
- [x] (sonnet-phase4-tests @ 2026-05-10) `pmcluster/e2e/webhook_test.go`: init → webhook add → serve → POST signed payload → 200 + stack deployed; wrong sig → 401; unknown source → 401; last_used_at populated; docker service ls verifies deployment.
- [ ] `pmcluster/e2e/registry_test.go`: pull a private image (use ttl.sh or a local registry container with auth) via a registry-credentialed deploy
- [ ] `pmcluster/e2e/credentials_test.go`: verify `pmcluster credentials show portainer` returns the same string that's loaded as the swarm secret

**Phase 4 test summary (sonnet-phase4-tests @ 2026-05-10):** Added 5 new test files and extended 2 existing files (25 new top-level test functions + 12 sub-tests in TestHandlerReceive). Files: `store/webhooks_test.go` (11 funcs), `store/registries_test.go` (8 funcs), `webhook/webhook_test.go` (3 top-level + 12 sub-tests covering all 401 cases with identical-body assertion, 200 path, 400, 502, 413, last_used_at), `cluster/credentials_test.go` extended with 4 Rotate tests, `cluster/fake_docker_test.go` extended with `secretRemoveErr` field + updated `SecretRemove`, `api/nodes_test.go` (3 funcs), `e2e/webhook_test.go` (e2e, build tag `e2e`). All pass `go test -race -count=1 ./...`.

---

### Phase 5 — Pre-deploy backup hook + CI hardening (was Phase 4)

**Goal:** Optional snapshot before deploy. Solid CI.

- [ ] DSL field `backup_before_deploy: true` (per-stack or per-volume list)
- [ ] `pmcluster/internal/backup/offen.go` — triggers offen via `docker exec` on the local backup container, or one-shot job. Investigate offen's signal/API surface in Phase 4 kickoff.
- [ ] Migration `0004_backups.sql` — `backups(id, stack_name, revision, volume, filename, size, created_at)`
- [ ] `GET /api/backups`, `POST /api/backups` (on-demand), `GET /api/stacks/{name}/backups`
- [ ] CLI: `pmcluster backup list`, `pmcluster backup create <stack> <volume>`
- [ ] CI workflow runs all unit + e2e on push to PR + main
- [ ] CI matrix: Go 1.22, 1.23
- [ ] CI: `golangci-lint run`
- [ ] **Restore is OUT OF SCOPE for this phase** — design doc only, write to `pmcluster/docs/restore-design.md`

#### Phase 4 tests
- [ ] Unit: backup orchestration logic with fake docker client
- [ ] e2e: deploy with `backup_before_deploy`, assert backup file appears on host

---

### Phase 6 — Brew formula + production polish (was Phase 5)

- [ ] `homebrew-tap/Formula/pmcluster.rb` (location TBD per Open Question 1):
  - [ ] Builds from source (or downloads release tarball)
  - [ ] Installs binary to `bin/pmcluster`
  - [ ] Service block for `brew services` (launchd on Mac, brew-services on Linux)
  - [ ] Caveats note: needs docker.sock access (group membership)
- [ ] systemd unit file template for non-brew Linux installs (`pmcluster/contrib/systemd/pmcluster.service`)
- [ ] GitHub Actions release workflow: tag → build cross-platform binaries (darwin/arm64, darwin/amd64, linux/arm64, linux/amd64) → release → SHA256 → bump formula
- [ ] Top-level `README.md` rewrite: install section, getting-started flow, link to RFC update
- [ ] RFC issue update (issue #1 in this repo): document the architectural shift from script-driven to pmcluster-driven
- [~] Migration guide: not shipped — user dropped this scope. v1 → v2 operators can git-checkout the pre-pmcluster commit if rollback is ever needed.

---

## Test Strategy

**Tooling:**
- `testing` stdlib + `github.com/stretchr/testify/require` (only `require`, no assertions library bloat)
- Golden file tests for the translator (`testdata/translator/<case>/dsl.yaml` ↔ `compose.yaml`)
- Mock the Docker client behind an internal interface; never touch the real socket in unit tests
- HTTP tests use `httptest.NewServer` against the real `chi.Router`

**E2E harness:**
- Container: `docker:24-dind` (privileged), pre-installs Go and the test binary
- Each test: fresh `dockerd` start, `docker swarm init`, build pmcluster, `pmcluster init`, run the scenario, assert
- `traefik/whoami` is the canonical dummy app
- Self-signed cert generated per-test, hostname routed via `--resolve`
- CI runs e2e in privileged GitHub Actions runners (ubuntu-latest supports this)

**Sub-agent test workflow:**
- When a code component is implemented and the implementer marks the task `[x]`, they spawn a sub-agent (Sonnet for non-trivial test logic, Haiku for mechanical golden-file generation) to write the corresponding unit tests
- Sub-agent prompt template lives in **Working Conventions** below
- Sub-agent updates the test checkbox `[x]` when done

---

## Working Conventions (read this before claiming a task)

### Status markers
- `[ ]` — open, claimable
- `[~] (agent-name @ YYYY-MM-DD)` — in progress, do not double-claim
- `[x] (agent-name @ YYYY-MM-DD)` — done
- `[!] (agent-name @ YYYY-MM-DD): <one-line note>` — blocked; see note

### Claiming a task
1. Read this whole plan file first.
2. Check that all tasks listed in `Decisions Already Made` are still respected.
3. Find an open `[ ]` task whose dependencies (earlier tasks in the same phase) are `[x]`.
4. Edit the plan file to mark `[~]` with your agent name and today's date.
5. Do the work. Commit when reasonable; don't batch unrelated work into one commit.
6. Mark `[x]` when truly done (compiles, lints, tests pass).
7. Spawn a sub-agent for the test task that pairs with what you finished (see template below).

### Sub-agent test prompt template

```
You are writing tests for a Go component that has just been implemented in the
pmcluster project at <repo>/pmcluster.

Component: <package path, e.g. internal/auth>
What it does: <1-2 sentences>
Public API: <list functions/types>

Write table-driven Go tests using stdlib `testing` + `github.com/stretchr/testify/require`.
Cover: <list cases — happy path, validation failures, edge cases>.

Conventions:
- Tests live next to the code (_test.go in the same package)
- Use `require`, not `assert`
- No external dependencies (no docker, no network) unless explicitly noted
- Mocks via small interfaces defined in the package under test

When done, run `go test ./<package>/...` and ensure it passes.
Then update the plan file at /Users/h.arian/.claude/plans/quiet-painting-iverson.md
to mark the corresponding test checkbox [x] with your agent name and date.

Report: list of files created, test count, any uncovered cases you noticed.
```

Use `subagent_type: general-purpose` with `model: sonnet` (default) or `model: haiku` for golden-file tests. Spawn in foreground when you need to verify before moving on; background when truly independent.

### What to do when blocked
- Mark task `[!]` with a one-line note about the blocker.
- Add a new entry under **Open Questions** at the bottom describing the blocker and what's needed to unblock.
- Move to the next claimable task.

### Don't
- Don't refactor anything outside your task scope.
- Don't change `Decisions Already Made` without raising a question to the user.
- Don't expand the DSL beyond what Phase 2 specifies until Phase 2 is complete.
- Don't add dependencies beyond the approved set (see decision row 11) without a question.

---

## Open Questions (resolve in Phase 0)

1. **Brew tap location.** Same-repo `homebrew-tap/` subdir vs separate `homebrew-tap` repo. Separate is the conventional Homebrew layout (`brew tap user/tap` resolves to `github.com/user/homebrew-tap`); same-repo is one less repo to manage early on. **Recommendation: separate repo, but defer creation to Phase 5.**

2. **Bootstrap sequencing.** `bin/setup.sh` currently creates the secrets (`admin_credentials`, `portainer_admin_password`, `zo_root_user_*`) that the stacks consume. Once pmcluster owns these (Phase 3), one of:
   - (a) `pmcluster init` MUST run before `bin/setup.sh` — clean separation
   - (b) `bin/setup.sh` keeps creating placeholder secrets in Phase 1, pmcluster takes them over in Phase 3 with a rotate
   - (c) `bin/setup.sh` becomes a thin wrapper that calls `pmcluster init` automatically
   **Recommendation: (a)** — install pmcluster first, then run `bin/setup.sh`. Documented clearly in README.

3. **`docker stack deploy` vs SDK ServiceCreate.** The Docker Go SDK doesn't have a high-level "deploy stack" function — you'd implement the same compose-parsing + per-service `ServiceCreate/Update` + secret/network/config orchestration that `docker cli` does internally. Two options:
   - (a) Shell out to `docker stack deploy -c -` from pmcluster, feeding the rendered compose YAML on stdin. Simple, battle-tested, has the downside of requiring the docker CLI on the manager host.
   - (b) Reimplement stack-deploy semantics using the SDK directly. More code, no CLI dependency, finer-grained control over which services to update.
   **Recommendation: (a) for Phase 2**, with an interface so we can swap to (b) later if needed. The docker CLI is universally present on a Swarm manager.

4. **CI runner privileged-mode availability.** GitHub-hosted `ubuntu-latest` supports privileged Docker, but if the user wants to run CI elsewhere (self-hosted, GitLab, etc.) this might need adjustment. Defer until the user picks a CI provider.

---

## Verification (end-to-end)

When all phases are complete, the following sequence should work on a fresh manager VM:

1. Operator: `apt install docker.io && docker swarm init --advertise-addr <ip>`
2. Operator: `git clone https://github.com/.../poor-man-stack && cd poor-man-stack`
3. Operator: `cp .env.example .env && vi .env` (DOMAIN, CERT_PATH, KEY_PATH only)
4. Operator: `brew install hazemarian/tap/pmcluster`
5. Operator: `pmcluster init` (prints bootstrap token, generates managed credentials, creates Swarm secrets)
6. Operator: `bin/setup.sh` (preflight checks pass, deploys infra/observability/backup stacks idempotently, generates OTel config via `pmcluster render`)
7. Operator: `brew services start pmcluster`
8. Operator visits `https://traefik.<domain>` (login from `pmcluster credentials show traefik_dashboard`), `https://portainer.<domain>` (creds from `pmcluster credentials show portainer`), `https://observ.<domain>` (creds from `pmcluster credentials show openobserve_admin`), `https://pmcluster.<domain>/health` (returns 200)
9. From a developer laptop: `curl -X POST https://pmcluster.<domain>/api/stacks -H 'Authorization: Bearer <token>' -d @donation-campaign.yaml` → app deploys → live at `https://api.donation-campaign.<domain>`
10. `curl -X POST https://pmcluster.<domain>/api/stacks/donation-campaign/rollback -d '{"revision":<earlier>}'` → reverts
11. From a webhook source: signed POST to `/webhook/github` → same deploy flow

E2E test in CI mirrors steps 4–11 inside a privileged dind container with a self-signed cert and resolved hostnames.

---

## Critical files (quick reference)

**Will be modified:**
- `bin/setup.sh` — Phase 1.1
- `worker-node/setup.sh` — Phase 1.1
- `main-node/setup.sh` — Phase 1.1 (likely deleted)
- `main-node/*.yml` → `stacks/*.yml` — Phase 1.1 (renamed)
- `stacks/traefik-dynamic.yml` — Phase 1.1 (add pmcluster route)
- `stacks/infra-stack.yml` — Phase 1.1 (`extra_hosts` on Traefik) + Phase 3 (Portainer secret no longer in setup.sh)
- `.env.example` — Phase 1.1
- `README.md` — Phase 1.9 + Phase 5

**Will be created:**
- Entire `pmcluster/` Go module (Phases 1–4)
- `homebrew-tap/Formula/pmcluster.rb` (Phase 5, possibly separate repo)
- `.github/workflows/pmcluster.yml` (Phase 1.8 + Phase 4)

**Stays untouched:**
- `stacks/observability-stack.yml`
- `stacks/backup-stack.yml`
- `stacks/otel-collector-config.yaml.template` (rendering moves to pmcluster but file content stays)
