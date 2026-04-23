#!/bin/bash
set -e

# ─────────────────────────────────────────────────────────────────────────────
# Poor Man's Cluster — unified setup script
# Usage: ./bin/setup.sh <manager|worker>
# ─────────────────────────────────────────────────────────────────────────────

MODE="${1:-}"
BIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$BIN_DIR/.." && pwd)"
STACKS_DIR="$REPO_DIR/main-node"

usage() {
    cat <<EOF
Usage: $(basename "$0") <manager|worker>

  manager   Initialise Docker Swarm, create secrets and networks,
            and deploy all stacks on this machine.

  worker    Install Docker and join an existing Swarm as a worker node.
            Requires MANAGER_IP and SWARM_JOIN_TOKEN in .env.

EOF
    exit 1
}

[[ "$MODE" == "manager" || "$MODE" == "worker" ]] || usage

# ── Shared helpers ────────────────────────────────────────────────────────────

install_docker() {
    if ! command -v docker &> /dev/null; then
        echo "🐳 Docker not found. Installing..."
        curl -fsSL https://get.docker.com -o get-docker.sh
        sh get-docker.sh
        rm get-docker.sh
    else
        echo "✅ Docker already installed."
    fi
}

load_env() {
    if [ ! -f "$REPO_DIR/.env" ]; then
        echo "📄 .env not found. Copying .env.example → .env"
        echo "   Fill in your values and run the script again."
        cp "$REPO_DIR/.env.example" "$REPO_DIR/.env"
        exit 1
    fi
    set -a
    # shellcheck source=/dev/null
    source "$REPO_DIR/.env"
    set +a
}

require_var() {
    local var="$1"
    if [ -z "${!var}" ]; then
        echo "❌ $var is not set in .env"
        exit 1
    fi
}

# ── Manager ───────────────────────────────────────────────────────────────────

setup_manager() {
    echo "🖥️  Setting up manager node..."

    install_docker

    # Initialise Swarm
    if [ "$(docker info --format '{{.Swarm.LocalNodeState}}')" != "active" ]; then
        echo "🌟 Initialising Docker Swarm..."
        IP_ADDR=$(hostname -I | awk '{print $1}')
        docker swarm init --advertise-addr "$IP_ADDR"
    else
        echo "✅ Swarm already active."
    fi

    # Tear down existing state
    echo "🔥 Removing existing stacks..."
    docker stack rm infra         || true
    docker stack rm observability || true
    docker stack rm backup        || true
    echo "⏳ Waiting for stacks to be removed... (10s)"
    sleep 10

    echo "🧹 Pruning stopped containers and unused volumes..."
    docker container prune -f || true
    docker volume prune -f    || true

    echo "🔒 Removing old secrets..."
    for secret in admin_credentials cert key portainer_admin_password \
                  zo_root_user_email zo_root_user_password; do
        docker secret rm "$secret" || true
    done

    echo "🌐 Removing old networks..."
    docker network rm traefik-net    || true
    docker network rm monitoring-net || true

    # Load and validate config
    load_env

    echo "ℹ️  Validating manager configuration..."
    for var in DOMAIN TRAEFIK_ADMIN_USER TRAEFIK_ADMIN_PASSWORD \
               ZO_ROOT_USER_EMAIL ZO_ROOT_USER_PASSWORD \
               PORTAINER_ADMIN_PASSWORD CERT_PATH KEY_PATH; do
        require_var "$var"
    done

    command -v openssl &> /dev/null || { echo "❌ openssl is not installed."; exit 1; }
    [ -f "$CERT_PATH" ] || { echo "❌ Certificate not found: $CERT_PATH"; exit 1; }
    [ -f "$KEY_PATH"  ] || { echo "❌ Key not found: $KEY_PATH"; exit 1; }

    KEY_MODULUS=$(openssl rsa   -noout -modulus -in "$KEY_PATH"  2>/dev/null | openssl md5)
    CERT_MODULUS=$(openssl x509 -noout -modulus -in "$CERT_PATH" 2>/dev/null | openssl md5)
    [ "$KEY_MODULUS" = "$CERT_MODULUS" ] || {
        echo "❌ Certificate and private key do not match."
        exit 1
    }
    echo "✅ Certificate and key match."

    # Networks
    echo "🌐 Creating overlay networks..."
    docker network create --driver=overlay --attachable traefik-net
    docker network create --driver=overlay --attachable monitoring-net

    # Secrets
    echo "🔒 Creating secrets..."
    HTPASSWD=$(echo -n "$TRAEFIK_ADMIN_PASSWORD" | openssl passwd -apr1 -stdin)
    echo -n "$TRAEFIK_ADMIN_USER:$HTPASSWD" | docker secret create admin_credentials -
    cat "$CERT_PATH"                         | docker secret create cert -
    cat "$KEY_PATH"                          | docker secret create key -
    echo -n "$PORTAINER_ADMIN_PASSWORD"      | docker secret create portainer_admin_password -
    echo -n "$ZO_ROOT_USER_EMAIL"            | docker secret create zo_root_user_email -
    echo -n "$ZO_ROOT_USER_PASSWORD"         | docker secret create zo_root_user_password -
    echo "✅ All secrets created."

    # Generate OTel config from template (keeps credentials out of git)
    echo "🛠️  Generating OTel Collector config..."
    local template="$STACKS_DIR/otel-collector-config.yaml.template"
    [ -f "$template" ] || { echo "❌ $template not found."; exit 1; }
    BASIC_AUTH=$(echo -n "$ZO_ROOT_USER_EMAIL:$ZO_ROOT_USER_PASSWORD" | base64)
    sed "s|__BASIC_AUTH_PLACEHOLDER__|Basic $BASIC_AUTH|g" \
        "$template" > "$STACKS_DIR/otel-collector-config.yaml"
    echo "✅ otel-collector-config.yaml generated."

    # Deploy stacks
    echo "🚀 Deploying Infrastructure (Traefik & Portainer)..."
    docker stack deploy --prune --resolve-image always -c "$STACKS_DIR/infra-stack.yml" infra

    echo "🚀 Deploying Observability..."
    docker stack deploy --prune --resolve-image always -c "$STACKS_DIR/observability-stack.yml" observability

    echo "🚀 Deploying Backups..."
    docker stack deploy --prune --resolve-image always -c "$STACKS_DIR/backup-stack.yml" backup

    echo ""
    echo "🎉 Manager ready! Dashboards:"
    echo "   Traefik:   https://traefik.$DOMAIN"
    echo "   Portainer: https://portainer.$DOMAIN"
    echo "   Observ:    https://observ.$DOMAIN"
    echo ""
    echo "To add worker nodes, copy this repo to each machine and run:"
    echo "   ./worker-node/setup.sh"
    echo "   (set MANAGER_IP and SWARM_JOIN_TOKEN in .env first)"
    echo ""
    echo "Get the worker join token with:  docker swarm join-token worker"
}

# ── Worker ────────────────────────────────────────────────────────────────────

setup_worker() {
    echo "⚙️  Setting up worker node..."

    install_docker

    load_env
    require_var MANAGER_IP
    require_var SWARM_JOIN_TOKEN

    echo ""
    echo "ℹ️  Ensure these ports are open between this node and the manager:"
    echo "   TCP 2377     — Cluster management"
    echo "   TCP/UDP 7946 — Node-to-node communication"
    echo "   UDP 4789     — Overlay network traffic (VXLAN)"
    echo ""

    SWARM_STATE=$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo "inactive")
    if [ "$SWARM_STATE" = "active" ]; then
        echo "✅ Already part of a Swarm. Skipping join."
    else
        echo "🌟 Joining Swarm as worker..."
        docker swarm join --token "$SWARM_JOIN_TOKEN" "$MANAGER_IP:2377"
        echo "✅ Joined the Swarm."
    fi

    echo "📁 Creating host directories for global services..."
    mkdir -p /var/backups/docker-volumes
    echo "✅ Host directories ready."

    echo ""
    echo "🎉 Worker node ready!"
    echo "   The manager will automatically schedule on this node:"
    echo "   - OTel Collector  (telemetry collection)"
    echo "   - Volume Backup   (nightly backups at 3:00 AM)"
    echo ""
    echo "   Verify from the manager:  docker node ls"
}

# ── Dispatch ──────────────────────────────────────────────────────────────────

case "$MODE" in
    manager) setup_manager ;;
    worker)  setup_worker  ;;
esac
