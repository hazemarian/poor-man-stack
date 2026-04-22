#!/bin/bash
set -e

# 1. Install Docker (if not present)
if ! command -v docker &> /dev/null; then
    echo "🐳 Docker not found. Installing..."
    curl -fsSL https://get.docker.com -o get-docker.sh
    sh get-docker.sh
    rm get-docker.sh
else
    echo "✅ Docker is already installed."
fi

# 2. Initialize Swarm (if not active)
if [ "$(docker info --format '{{.Swarm.LocalNodeState}}')" != "active" ]; then
    echo "🌟 Initializing Docker Swarm..."
    IP_ADDR=$(hostname -I | awk '{print $1}')
    docker swarm init --advertise-addr "$IP_ADDR"
else
    echo "✅ Swarm is already active."
fi

# 3. Remove existing stacks, secrets & networks
echo "🔥 Removing existing stacks..."
docker stack rm infra         || true
docker stack rm observability || true
docker stack rm backup        || true
echo "⏳ Waiting for stacks to be removed... (10s)"
sleep 10

echo "🧹 Cleaning up old containers and volumes..."
docker container prune -f || true
docker volume prune -f    || true

echo "🔒 Removing old secrets..."
docker secret rm admin_credentials        || true
docker secret rm cert                     || true
docker secret rm key                      || true
docker secret rm portainer_admin_password || true
docker secret rm zo_root_user_email       || true
docker secret rm zo_root_user_password    || true

echo "🌐 Removing existing networks..."
docker network rm traefik-net    || true
docker network rm monitoring-net || true

# 4. Create networks
echo "🌐 Creating overlay networks..."
docker network create --driver=overlay --attachable traefik-net
docker network create --driver=overlay --attachable monitoring-net

# 5. Load configuration
if [ ! -f .env ]; then
    echo "📄 .env not found. Copying .env.example to .env."
    echo "Please edit .env with your configuration and run the script again."
    cp .env.example .env
    exit 1
fi
set -a
source .env
set +a

echo "ℹ️  Configuration loaded from .env:"
echo "    DOMAIN, TRAEFIK_ADMIN_USER, TRAEFIK_ADMIN_PASSWORD"
echo "    ZO_ROOT_USER_EMAIL, ZO_ROOT_USER_PASSWORD"
echo "    PORTAINER_ADMIN_PASSWORD, CERT_PATH, KEY_PATH"
echo ""

# 6. Validate required variables
if ! command -v openssl &> /dev/null; then
    echo "❌ openssl is not installed. Please install it to continue."
    exit 1
fi

for var in DOMAIN TRAEFIK_ADMIN_USER TRAEFIK_ADMIN_PASSWORD \
           ZO_ROOT_USER_EMAIL ZO_ROOT_USER_PASSWORD \
           PORTAINER_ADMIN_PASSWORD CERT_PATH KEY_PATH; do
    if [ -z "${!var}" ]; then
        echo "❌ Error: $var is not set in the .env file."
        exit 1
    fi
done

if [ ! -f "$CERT_PATH" ]; then
    echo "❌ Certificate file not found: $CERT_PATH"
    exit 1
fi

if [ ! -f "$KEY_PATH" ]; then
    echo "❌ Key file not found: $KEY_PATH"
    exit 1
fi

KEY_MODULUS=$(openssl rsa  -noout -modulus -in "$KEY_PATH"  2>/dev/null | openssl md5)
CERT_MODULUS=$(openssl x509 -noout -modulus -in "$CERT_PATH" 2>/dev/null | openssl md5)

if [ "$KEY_MODULUS" != "$CERT_MODULUS" ]; then
    echo "❌ Certificate and private key do not match."
    echo "   Ensure CERT_PATH and KEY_PATH point to a matching certificate and key."
    exit 1
fi
echo "✅ Certificate and key match."

# 7. Create secrets
echo "🔒 Creating secrets..."

HTPASSWD=$(echo -n "$TRAEFIK_ADMIN_PASSWORD" | openssl passwd -apr1 -stdin)
echo -n "$TRAEFIK_ADMIN_USER:$HTPASSWD" | docker secret create admin_credentials -
echo "✅ Secret 'admin_credentials' created."

cat "$CERT_PATH" | docker secret create cert -
echo "✅ Secret 'cert' created."

cat "$KEY_PATH" | docker secret create key -
echo "✅ Secret 'key' created."

echo -n "$PORTAINER_ADMIN_PASSWORD" | docker secret create portainer_admin_password -
echo "✅ Secret 'portainer_admin_password' created."

echo -n "$ZO_ROOT_USER_EMAIL" | docker secret create zo_root_user_email -
echo "✅ Secret 'zo_root_user_email' created."

echo -n "$ZO_ROOT_USER_PASSWORD" | docker secret create zo_root_user_password -
echo "✅ Secret 'zo_root_user_password' created."

# 8. Generate OTel Collector config from template
# The template contains a placeholder for the auth token.
# We generate the real config here so credentials never touch git.
echo "🛠️  Generating OTel Collector config..."

if [ ! -f otel-collector-config.yaml.template ]; then
    echo "❌ otel-collector-config.yaml.template not found."
    exit 1
fi

BASIC_AUTH=$(echo -n "$ZO_ROOT_USER_EMAIL:$ZO_ROOT_USER_PASSWORD" | base64)
sed "s|__BASIC_AUTH_PLACEHOLDER__|Basic $BASIC_AUTH|g" \
    otel-collector-config.yaml.template > otel-collector-config.yaml

echo "✅ otel-collector-config.yaml generated."

# 9. Deploy stacks
echo "🚀 Deploying Infrastructure (Traefik & Portainer)..."
docker stack deploy --prune --resolve-image always -c infra-stack.yml infra

echo "🚀 Deploying Observability..."
docker stack deploy --prune --resolve-image always -c observability-stack.yml observability

echo "🚀 Deploying Backups..."
docker stack deploy --prune --resolve-image always -c backup-stack.yml backup

echo ""
echo "🎉 Done! Dashboard URLs:"
echo "   Traefik:   https://traefik.$DOMAIN"
echo "   Portainer: https://portainer.$DOMAIN"
echo "   Observ:    https://observ.$DOMAIN"
