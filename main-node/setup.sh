#!/bin/bash

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
    # Auto-detect the primary IP address of the server
    IP_ADDR=$(hostname -I | awk '{print $1}')
    docker swarm init --advertise-addr $IP_ADDR
else
    echo "✅ Swarm is already active."
fi

# 3. Remove Existing Stacks, Secrets & Networks

echo "🔥 Removing existing stacks to release networks..."
docker stack rm infra || true
docker stack rm observability || true
docker stack rm backup || true
echo "⏳ Waiting for stacks to be removed... (15s)"
sleep 10

echo "🧹 Cleaning up old containers and volumes..."
docker container prune -f || true
docker volume prune -f || true

echo "🔒 Removing secrets..."
docker secret rm admin_credentials || true
docker secret rm cert || true
docker secret rm key || true
docker secret rm portainer_admin_password || true

echo "🌐 Removing existing networks..."
docker network rm traefik-net || true
docker network rm monitoring-net || true

# 4. Create Networks

echo "🌐 Creating fresh overlay networks..."
docker network create --driver=overlay --attachable traefik-net
docker network create --driver=overlay --attachable monitoring-net

# 5. Load Configuration
if [ ! -f .env ]; then
    echo "📄 .env file not found. Copying .env.example to .env."
    echo "Please edit the .env file with your configuration and run the script again."
    cp .env.example .env
    exit 1
fi
set -a
source .env
set +a

echo "ℹ️  Using the following configuration from .env file:"
echo "    - DOMAIN"
echo "    - TRAEFIK_ADMIN_USER"
echo "    - TRAEFIK_ADMIN_PASSWORD"
echo "    - ZO_ROOT_USER_EMAIL"
echo "    - ZO_ROOT_USER_PASSWORD"
echo "    - PORTAINER_ADMIN_PASSWORD"
echo "    - CERT_PATH"
echo "    - KEY_PATH"
echo ""

# Validate required variables

if ! command -v openssl &> /dev/null; then

    echo "Error: openssl is not installed. Please install it to continue."

    exit 1

fi



if [ -z "$DOMAIN" ]; then

  echo "❌ Error: DOMAIN is not set in the .env file."

  exit 1

fi

if [ -z "$TRAEFIK_ADMIN_USER" ]; then

  echo "❌ Error: TRAEFIK_ADMIN_USER is not set in the .env file."

  exit 1

fi

if [ -z "$TRAEFIK_ADMIN_PASSWORD" ]; then

  echo "❌ Error: TRAEFIK_ADMIN_PASSWORD is not set in the .env file."

  exit 1

fi

if [ -z "$ZO_ROOT_USER_EMAIL" ]; then
    
    echo "❌ Error: ZO_ROOT_USER_EMAIL is not set in the .env file."
    
    exit 1
    
fi

if [ -z "$ZO_ROOT_USER_PASSWORD" ]; then
    
    echo "❌ Error: ZO_ROOT_USER_PASSWORD is not set in the .env file."
    
    exit 1
    
fi

if [ -z "$PORTAINER_ADMIN_PASSWORD" ]; then

  echo "❌ Error: PORTAINER_ADMIN_PASSWORD is not set in the .env file."

  exit 1

fi

if [ -z "$CERT_PATH" ]; then

  echo "❌ Error: CERT_PATH is not set in the .env file."

  exit 1

fi

if [ ! -f "$CERT_PATH" ]; then

  echo "❌ Error: Certificate file not found at path specified by CERT_PATH: $CERT_PATH"

  exit 1

fi

if [ -z "$KEY_PATH" ]; then

  echo "❌ Error: KEY_PATH is not set in the .env file."

  exit 1

fi

if [ ! -f "$KEY_PATH" ]; then

  echo "❌ Error: Key file not found at path specified by KEY_PATH: $KEY_PATH"

  exit 1

fi



KEY_MODULUS=$(openssl rsa -noout -modulus -in "$KEY_PATH" 2>/dev/null | openssl md5)

CERT_MODULUS=$(openssl x509 -noout -modulus -in "$CERT_PATH" 2>/dev/null | openssl md5)



if [ "$KEY_MODULUS" != "$CERT_MODULUS" ]; then

  echo "❌ Error: Certificate and private key do not match."

  echo "Please ensure CERT_PATH and KEY_PATH point to a valid certificate and its corresponding private key."

  exit 1

fi

echo "✅ Certificate and key match."





# 6. Create Secrets



echo "🔒 Creating secrets..."



HTPASSWD=$(echo -n "$TRAEFIK_ADMIN_PASSWORD" | openssl passwd -apr1 -stdin)

HTPASSWD_SECRET_CONTENT="$TRAEFIK_ADMIN_USER:$HTPASSWD"

echo -n "$HTPASSWD_SECRET_CONTENT" | docker secret create admin_credentials -

# The command below reads the content of the file specified by the $CERT_PATH
# environment variable and uses it to create the 'cert' secret.

cat "$CERT_PATH" | docker secret create cert -
echo "✅ Secret 'cert' created.  $CERT_PATH"

# Similarly, this command reads the content from the file at $KEY_PATH
# to create the 'key' secret.
cat "$KEY_PATH" | docker secret create key -
echo "✅ Secret 'key' created. $KEY_PATH"


echo -n "$PORTAINER_ADMIN_PASSWORD" | docker secret create portainer_admin_password -
echo "✅ Secret 'portainer_admin_password' created."


# 7. Configure OTel Collector
echo "🛠️ Configuring OTel Collector..."

# Create a backup of the original file
cp otel-collector-config.yaml otel-collector-config.yaml.bak

# Generate the Basic Auth string
BASIC_AUTH=$(echo -n "$ZO_ROOT_USER_EMAIL:$ZO_ROOT_USER_PASSWORD" | base64)

# Replace the placeholder with the actual Basic Auth string
sed -i.bak "s|__BASIC_AUTH_PLACEHOLDER__|Basic $BASIC_AUTH|g" otel-collector-config.yaml

echo "✅ OTel Collector configured."


# 8. Deploy the Stacks
echo "🚀 Deploying Infrastructure (Traefik & Portainer)..."
docker stack deploy --prune --resolve-image always -c infra-stack.yml infra

echo "🚀 Deploying Observability ..."
docker stack deploy --prune --resolve-image always -c observability-stack.yml observability

echo "🚀 Deploying Backups..."
docker stack deploy --prune --resolve-image always -c backup-stack.yml backup

echo "🎉 DONE! Dashboard URLs:"
echo "   - Traefik:   https://traefik.$DOMAIN"
echo "   - Portainer: https://portainer.$DOMAIN"
echo "   - Observ:    https://observ.$DOMAIN"
