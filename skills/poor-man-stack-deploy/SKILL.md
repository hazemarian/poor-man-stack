---
name: poor-man-stack-deploy
description: >
  Deploy services onto a Poor Man's Stack cluster using the pmcluster CLI or REST API.
  Covers the pmcluster DSL manifest format, deploy/rollback/list/show commands, webhook
  setup for CI, registry credential management, backup triggering, and cluster lifecycle
  (up/down/status). Use when deploying applications to a Docker Swarm-based cluster
  managed by pmcluster, or when setting up CI/CD pipelines targeting pmcluster.
  Do NOT use for Docker/Kubernetes deployments outside this stack.
---

# Poor Man's Stack — Deploy Skill

Deploy applications to a Docker Swarm cluster managed by `pmcluster`, the control plane from [poor-man-stack](https://github.com/hazemarian/poor-man-stack).

## Prerequisites

Before any deploy operation, verify the environment:

```bash
# Is Docker running and Swarm active?
docker info --format '{{.Swarm.LocalNodeState}}'   # must be "active"

# Is pmcluster installed and initialized?
pmcluster version
pmcluster cluster status
```

If pmcluster is not installed:

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-stack/main/install.sh | bash
pmcluster init
```

If the cluster is not up:

```bash
pmcluster cluster up --domain=<your-domain> --cert=<cert.pem> --key=<key.pem> --openobserve-email=<you@host>
# OR with Let's Encrypt:
pmcluster cluster up --domain=<your-domain> --acme-email=<you@host> --openobserve-email=<you@host>
```

The daemon must be running for the REST API:

```bash
pmcluster serve   # foreground; supervise via systemd for production
```

## Manifest DSL Format

Create a `.yaml` manifest for each service. The schema:

```yaml
app: my-app                    # required — application name
env: production                # required — environment (staging/production/etc.)
domain: example.com            # required — root domain for Traefik routing
registry: ghcr.io/my-org       # optional — container registry
version: latest                # optional — default image tag (overridable at deploy)

backup_before_deploy: true     # optional — trigger offen volume snapshot before deploy

secrets:                       # optional — external Swarm secrets (must already exist)
  - my_app_db_password

volumes:                       # optional — named volumes to create
  - db_data

services:                      # required — one or more service definitions
  service-name:
    image: postgres:14-alpine
    # OR with variable substitution:
    # image: ${registry}/${app}:${version}

    placement: manager         # optional — constrain to manager node

    replicas: 2                # optional — default 1

    command: [./migrate]       # optional — override entrypoint/command

    run_once: true             # optional — restart_policy: condition: none (for jobs/migrations)

    expose:                    # optional — auto-wire Traefik route
      port: 8080
      host: api.${app}.${domain}   # variable substitution available

    volumes:                   # optional — volume mounts
      - db_data:/var/lib/postgresql/data

    env:                       # optional — environment variables
      POSTGRES_DB: my_app
      POSTGRES_USER: my_user

    secrets:                   # optional — attach secrets
      - my_app_db_password

    healthcheck:               # optional
      type: pg_isready         # or: http (requires path: /health)
```

### Variable Substitution

These variables are auto-resolved: `${app}`, `${env}`, `${version}`, `${registry}`, `${domain}`. 

OS environment variables: `${env:VAR_NAME}`.

### Validation Rules

- Strict YAML — unknown top-level keys are rejected.
- `app`, `env`, `domain`, `services` are required.
- Unknown keys inside a service definition are rejected.
- `expose` requires both `port` and `host`.
- `healthcheck.type` must be `http` (with `path`) or `pg_isready`.

## Deploy Workflow

### 1. Create the manifest

Write a `.yaml` manifest file for your service. Start from the template above.

### 2. Validate (dry-run)

```bash
pmcluster deploy ./manifest.yaml --dry-run
```

This translates the DSL to Docker Compose and shows what would be applied without touching Swarm.

### 3. Deploy

```bash
pmcluster deploy ./manifest.yaml

# Override version at deploy time:
pmcluster deploy ./manifest.yaml --version v1.2.3

# Override app name:
pmcluster deploy ./manifest.yaml --app custom-name
```

Each deploy creates a new revision keyed by Unix timestamp.

### 4. Verify

```bash
pmcluster stack list                        # all deployed stacks
pmcluster stack show my-app                 # metadata + revision history (→ marks current)
docker service ls                           # raw Swarm view
docker service ps my-app_api                # container-level status
```

## Rollback

```bash
pmcluster stack show my-app                 # find the revision you want
pmcluster rollback my-app 1719000000        # redeploy that revision
```

Rollback creates a NEW revision (preserves audit trail) pointing at the old compose. Both forward and backward deploys are recorded.

## REST API Deploy (Remote / CI)

When deploying from CI or a remote machine, use the REST API:

```bash
# Deploy a manifest
curl -X POST https://pmcluster.example.com/api/stacks \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d "{\"manifest\": $(jq -Rs . < manifest.yaml)}"

# List stacks
curl https://pmcluster.example.com/api/stacks \
  -H "Authorization: Bearer <admin-token>"

# Show a stack
curl https://pmcluster.example.com/api/stacks/my-app \
  -H "Authorization: Bearer <admin-token>"

# Rollback
curl -X POST https://pmcluster.example.com/api/stacks/my-app/rollback \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"revision": 1719000000}'
```

## CI/CD Webhook Setup

### 1. Create a webhook source on the manager

```bash
pmcluster webhook add github-prod
# Prints a 64-char hex secret ONCE — save it.
```

### 2. Configure CI (e.g., GitHub Actions)

```yaml
jobs:
  deploy:
    steps:
      - name: Deploy to pmcluster
        run: |
          BODY='{"app": "my-app", "version": "${{ github.sha }}", "manifest": "'$(cat manifest.yaml | jq -Rs . | cut -c2- | rev | cut -c2- | rev)'"}'
          SIG=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "${{ secrets.PMCLUSTER_WEBHOOK_SECRET }}" | awk '{print "sha256=" $2}')
          curl -X POST https://pmcluster.example.com/webhook/github-prod \
            -H "X-Pmcluster-Signature: $SIG" \
            -H "Content-Type: application/json" \
            -d "$BODY"
```

The signature is HMAC-SHA256 over the request body, hex-encoded, prefixed with `sha256=`.

### 3. Verify webhook

The webhook `source` is the name you gave it (`github-prod`). Requests with wrong secret, wrong source, or missing signature all return a generic 401 — no information leak.

List webhooks: `pmcluster webhook list`
Remove: `pmcluster webhook remove github-prod`

## Private Registries

If your images are in a private registry (e.g. `ghcr.io`):

```bash
pmcluster registry add ghcr.io            # prompts for username + password
pmcluster registry list                   # verify
```

`pmcluster serve` auto-replays `docker login` for all persisted registries on startup, so worker nodes can pull private images even after a manager rebuild.

## Pre-Deploy Backups

Add `backup_before_deploy: true` to a manifest to trigger an offen volume snapshot before deployment. The deploy proceeds even if the backup fails (flaky backup shouldn't block urgent rollouts):

```bash
pmcluster backup list                      # audit log of every triggered run
pmcluster backup create                    # on-demand snapshot
```

## Cluster Management

```bash
pmcluster cluster status                   # health overview
pmcluster cluster down --yes               # remove all stacks
pmcluster cluster down --yes --purge       # also remove secrets, configs, networks
pmcluster node list                        # Swarm nodes
pmcluster node join-token worker           # get join token for new workers
```

## Credentials

```bash
pmcluster credentials list                 # all managed bootstrap passwords
pmcluster credentials show portainer       # show a specific one
pmcluster credentials rotate portainer     # generate + apply a new password
```

## Audit Logs

```bash
pmcluster logs --tail=200                  # recent JSON log lines
pmcluster logs --since=24h --follow        # stream in real time
pmcluster logs --tail=200 | jq 'select(.level=="error")'
```

Log files live at `~/.pmcluster/logs/pmcluster-YYYY-MM-DD.log` and auto-sweep after 14 days.

## Troubleshooting

### "Swarm not active"
Run `docker swarm init --advertise-addr <ip>` on the manager. Pmcluster never initializes Swarm for you.

### "Docker not reachable"
Ensure Docker engine is running and the current user has permission (`docker info`).

### "secret already exists"
`pmcluster cluster up` is idempotent — re-run it. It reconciles, never destroys.

### "no such stack"
Run `pmcluster stack list` to see deployed stacks. Stack names come from the `app` field in manifests.

### Deploy fails
Check the Swarm service logs: `docker service logs my-app_api --tail=50`. Inspect the translated compose: `pmcluster deploy ./manifest.yaml --dry-run`.

### Worker node can't pull images
Ensure `pmcluster serve` is running (it replays registry credentials). Verify the registry was added: `pmcluster registry list`.
