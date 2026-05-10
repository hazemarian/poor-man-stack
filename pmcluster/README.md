# pmcluster

Single-binary control plane for the [poor-man-stack](../) Docker Swarm cluster.
Replaces the old `bin/setup.sh` and Portainer's GitOps role with a Cobra CLI
and an HTTP daemon (REST API + webhook receiver).

## Status

This project is under active refactor. See [`docs/refactor-plan.md`](../docs/refactor-plan.md)
for the phased plan, current progress, and conventions for picking up open tasks.

## Build

```bash
cd pmcluster
make build         # → ./bin/pmcluster
./bin/pmcluster --help
./bin/pmcluster version
```

`make help` lists the other targets (`test`, `e2e`, `lint`, `tidy`, `fmt`, `vet`, `clean`, `install`).

## Layout

```
cmd/pmcluster/         entry point
internal/
  cli/                 Cobra command tree (init, serve, cluster, version)
  config/              viper-backed config loading
  store/               SQLite (modernc.org/sqlite) + embedded migrations
  auth/                bearer tokens, argon2id hashing
  server/              chi HTTP server wiring
  api/                 REST handlers
  webhook/             webhook receivers (HMAC verification)
  docker/              Docker SDK wrapper
  cluster/             cluster lifecycle: preflight, secrets, networks, stacks
  credentials/         AES-GCM-encrypted credential storage
  manifest/            DSL parser + translator → Docker Swarm Compose
  registry/            registry credential storage
pkg/dsl/               public DSL types
migrations/            *.sql, embedded via //go:embed
testdata/              golden DSL ↔ compose pairs
e2e/                   docker-in-docker end-to-end tests
```

## Phase 1 status (scaffolding)

The CLI surface exists but most commands are skeletons that exit with a
"not implemented yet — lands in Phase X" message. The plan tracks which
phase owns each surface.
