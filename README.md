# Poor Man's Cluster

A simple, cost-effective, and production-ready deployment stack built on open-source tools. No Kubernetes, no managed cloud services, no expensive licensing — just Docker Swarm and a handful of well-chosen tools that get the job done.

This project gives you a full production cluster with ingress, observability, GitOps deployments, and automated backups, all configured and ready to run from a single setup script.

---

## How It Works

The stack runs on Docker Swarm. One machine acts as the **manager node** — it controls the cluster, hosts the management UIs, and deploys stacks. Any number of **worker nodes** can be added by running a single script; the manager automatically schedules services across them.

Two overlay networks connect everything:

- **`traefik-net`** — carries application traffic between Traefik and your services
- **`monitoring-net`** — carries telemetry (logs, metrics, traces) between services and OpenObserve

All sensitive credentials (passwords, TLS certificates) are stored as Docker Swarm secrets — encrypted at rest and in transit, never written to disk or environment variables.

Deployments are GitOps-driven: push a change to your Git repository and Portainer automatically pulls and redeploys the affected stack.

```
Internet
   │
   ▼
Traefik (HTTPS ingress, auto-routes via Docker labels)
   ├──▶ Your App(s)
   ├──▶ Portainer (management UI)
   └──▶ OpenObserve (observability UI)

Your App(s) ──OTLP──▶ OTel Collector ──▶ OpenObserve
Traefik     ──OTLP──▶ OTel Collector ──▶ OpenObserve

Every node: OTel Collector + Backup Agent (global services)
```

---

## Stack Components

### Traefik — Ingress & API Gateway
Traefik sits at the edge and routes all HTTPS traffic to the right service based on Docker labels. No config files to edit when you add a new service — just add labels to your Compose file and Traefik picks it up automatically. TLS certificates are loaded from Docker Swarm secrets. Built-in OpenTelemetry support sends traces and metrics to the collector.

### Portainer — Management UI & GitOps Controller
Portainer provides a web UI for the entire cluster. More importantly, it acts as the GitOps controller: point it at a Git repository containing your Compose files and it will watch for changes and redeploy automatically. Secrets, stacks, and node management are all available from a single interface.

### OpenObserve — Observability (Logs, Metrics, Traces)
A single lightweight binary that replaces the entire Grafana + Loki + Prometheus + Tempo stack. It ingests logs, metrics, and traces via OTLP and stores them efficiently (approximately 140x lower storage cost than Elasticsearch-based stacks). All three signal types are queryable in one UI.

### OpenTelemetry Collector — Telemetry Aggregation
Runs as a global service on every node in the cluster. It automatically discovers all running containers via the Docker observer, tails their logs, enriches them with Swarm metadata (service name, stack, node ID), collects Docker resource metrics (CPU, memory, network), and forwards everything to OpenObserve via OTLP.

### offen/docker-volume-backup — Automated Backups
Runs as a global service on every node. Performs nightly backups of all Docker volumes on that node, names them with the node ID to avoid conflicts, retains 7 days of history, and can optionally push archives to any S3-compatible object store.

---

## Repository Structure

```
poor-man-stack/
├── main-node/               # Manager node — run setup.sh here first
│   ├── setup.sh             # One-shot setup: installs Docker, creates secrets, deploys stacks
│   ├── .env.example         # Copy to .env and fill in your values
│   ├── infra-stack.yml      # Traefik + Portainer
│   ├── observability-stack.yml  # OpenObserve + OTel Collector
│   ├── backup-stack.yml     # Volume backup agent
│   ├── otel-collector-config.yaml  # OTel Collector pipeline config
│   └── traefik-dynamic.yml  # TLS certificate wiring for Traefik
│
└── worker-node/             # Worker nodes — run setup.sh on each additional machine
    ├── setup.sh             # Joins the Swarm and prepares host directories
    └── .env.example         # MANAGER_IP + SWARM_JOIN_TOKEN
```

---

## Getting Started

### 1. Manager Node

```bash
cd main-node
cp .env.example .env
# Edit .env with your domain, credentials, and TLS certificate paths
./setup.sh
```

The script will:
1. Install Docker if not present
2. Initialise Docker Swarm
3. Create overlay networks (`traefik-net`, `monitoring-net`)
4. Create all Docker Swarm secrets from your `.env` values
5. Deploy the infrastructure, observability, and backup stacks

Once complete, your dashboards will be live at:
- `https://traefik.<your-domain>` — Traefik dashboard
- `https://portainer.<your-domain>` — Portainer
- `https://observ.<your-domain>` — OpenObserve

### 2. Worker Nodes (optional)

On each additional machine:

```bash
cd worker-node
cp .env.example .env
# Set MANAGER_IP and SWARM_JOIN_TOKEN (get the token from the manager: docker swarm join-token worker)
./setup.sh
```

The worker joins the Swarm and the manager automatically schedules the OTel Collector and backup agent on it.

### Required Firewall Ports (manager ↔ worker)

| Port | Protocol | Purpose |
|------|----------|---------|
| 2377 | TCP | Swarm cluster management |
| 7946 | TCP/UDP | Node-to-node communication |
| 4789 | UDP | Overlay network traffic (VXLAN) |

---

## Deploying Your Own Services

Add your service to a Docker Compose file and include Traefik labels:

```yaml
services:
  my-app:
    image: my-image
    networks:
      - traefik-net
      - monitoring-net
    deploy:
      labels:
        - "traefik.enable=true"
        - "traefik.http.routers.my-app.rule=Host(`app.example.com`)"
        - "traefik.http.routers.my-app.entrypoints=websecure"
        - "traefik.http.routers.my-app.tls=true"
        - "traefik.http.services.my-app.loadbalancer.server.port=3000"

networks:
  traefik-net:
    external: true
  monitoring-net:
    external: true
```

Push the file to your Git repository and Portainer deploys it automatically.

---

## License

This project is licensed under the **MIT License**.

```
MIT License

Copyright (c) 2025 Hazem Arian

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

---

## Contact

Built and maintained by **Hazem Arian**.

- Email: [hazeem.arian@gmail.com](mailto:hazeem.arian@gmail.com)
- LinkedIn: [linkedin.com/in/hazem-a-467b4183](https://www.linkedin.com/in/hazem-a-467b4183/)

Contributions, issues, and feedback are welcome.
