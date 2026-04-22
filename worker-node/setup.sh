#!/bin/bash
set -e

# Load config
if [ ! -f .env ]; then
    echo "❌ .env file not found. Copying .env.example to .env."
    echo "Please fill in MANAGER_IP and SWARM_JOIN_TOKEN, then run again."
    cp .env.example .env
    exit 1
fi
set -a
source .env
set +a

# Validate required variables
if [ -z "$MANAGER_IP" ]; then
    echo "❌ Error: MANAGER_IP is not set in the .env file."
    exit 1
fi

if [ -z "$SWARM_JOIN_TOKEN" ]; then
    echo "❌ Error: SWARM_JOIN_TOKEN is not set in the .env file."
    echo "   Run this on the manager node to get it:"
    echo "   docker swarm join-token worker"
    exit 1
fi

# 1. Install Docker (if not present)
if ! command -v docker &> /dev/null; then
    echo "🐳 Docker not found. Installing..."
    curl -fsSL https://get.docker.com -o get-docker.sh
    sh get-docker.sh
    rm get-docker.sh
else
    echo "✅ Docker is already installed."
fi

# 2. Check firewall ports needed for Swarm communication
echo ""
echo "ℹ️  Ensure the following ports are open between this node and the manager:"
echo "   TCP 2377  - Cluster management"
echo "   TCP/UDP 7946 - Node-to-node communication"
echo "   UDP 4789  - Overlay network (VXLAN)"
echo ""

# 3. Join the Swarm
SWARM_STATE=$(docker info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo "inactive")

if [ "$SWARM_STATE" == "active" ]; then
    echo "✅ This node is already part of a Swarm. Skipping join."
else
    echo "🌟 Joining Docker Swarm as a worker..."
    docker swarm join --token "$SWARM_JOIN_TOKEN" "$MANAGER_IP:2377"
    echo "✅ Successfully joined the Swarm."
fi

# 4. Create host directories mounted by global services
echo "📁 Creating host directories for global services..."

# OTel Collector reads container logs from here (already exists on Docker hosts, but ensure it)
mkdir -p /var/lib/docker/containers

# Backup agent writes archives here
mkdir -p /var/backups/docker-volumes

echo "✅ Host directories ready."

echo ""
echo "🎉 Worker node setup complete!"
echo "   The manager will automatically schedule the following global services on this node:"
echo "   - OTel Collector  (telemetry collection)"
echo "   - Volume Backup   (nightly backups at 3:00 AM)"
echo ""
echo "   Verify this node appears in the cluster by running on the manager:"
echo "   docker node ls"
