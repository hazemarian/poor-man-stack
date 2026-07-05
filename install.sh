#!/usr/bin/env bash
# install.sh — fetch the latest pmcluster release binary and drop it in
# /usr/local/bin (override with PREFIX=/path/to/dir).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-stack/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-stack/main/install.sh | VERSION=v0.2.0 bash
#
# Optional env vars:
#   VERSION=v0.2.0          pin a specific release (default: latest)
#   PREFIX=/opt/bin         install location (default: /usr/local/bin)
#   PMCLUSTER_USER=deployer systemd service user (default: auto-detected)
#   PMCLUSTER_REGISTRY=host=user=token,...  auto-configure registries
#     (GHCR: PMCLUSTER_REGISTRY=ghcr.io=USERNAME=ghp_...)

set -euo pipefail

REPO="hazemarian/poor-man-stack"
PREFIX="${PREFIX:-/usr/local/bin}"
VERSION="${VERSION:-latest}"

# Detect OS and arch in the same shape the release workflow uses.
case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  echo "→ Resolving latest release from github.com/${REPO}"
  API_RESPONSE=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")
  if [ -z "$API_RESPONSE" ]; then
    echo "could not reach GitHub API (rate-limited or network error). Try:" >&2
    echo "  curl -fsSL .../install.sh | VERSION=v0.0.1 bash" >&2
    exit 1
  fi
  VERSION=$(echo "$API_RESPONSE" \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
  if [ -z "$VERSION" ]; then
    echo "could not resolve latest release tag" >&2
    exit 1
  fi
fi

ARCHIVE="pmcluster-${VERSION}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

echo "→ Downloading ${URL}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" -o "$TMP/$ARCHIVE"

# Verify checksum. The release workflow ships a SHA256SUMS.txt covering
# every archive; we grab the line for our archive and feed it to shasum.
echo "→ Verifying checksum"
curl -fsSL "https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.txt" \
  -o "$TMP/SHA256SUMS.txt"
(cd "$TMP" && grep " ${ARCHIVE}\$" SHA256SUMS.txt | shasum -a 256 -c -) \
  || { echo "checksum verification failed" >&2; exit 1; }

tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
BIN="$TMP/pmcluster-${VERSION}-${OS}-${ARCH}"

mkdir -p "$PREFIX"
if [ -w "$PREFIX" ]; then
  install -m 0755 "$BIN" "$PREFIX/pmcluster"
else
  echo "→ ${PREFIX} requires sudo"
  sudo install -m 0755 "$BIN" "$PREFIX/pmcluster"
fi

echo
echo "✅ pmcluster ${VERSION} installed at ${PREFIX}/pmcluster"
# Ensure backup destination exists (bind mount in backup stack).
if [ ! -d /var/backups/docker-volumes ]; then
  mkdir -p /var/backups/docker-volumes 2>/dev/null || true
fi

echo
"$PREFIX/pmcluster" version || true
echo

# --- Auto-configure registries (if PMCLUSTER_REGISTRY is set) ---
configure_registries() {
  local spec="${1:-}"
  [ -z "$spec" ] && return

  # Split on comma: host=user=token,host2=user2=token2,...
  IFS=',' read -ra ENTRIES <<<"$spec"
  for entry in "${ENTRIES[@]}"; do
    # Trim whitespace.
    entry="$(echo "$entry" | xargs)"
    [ -z "$entry" ] && continue

    # Parse host, user, token (token is everything after the second =).
    host="${entry%%=*}"
    rest="${entry#*=}"
    user="${rest%%=*}"
    token="${rest#*=}"

    if [ -z "$host" ] || [ -z "$user" ] || [ -z "$token" ]; then
      echo "⚠  Skipping malformed registry entry: $entry (expect host=user=token)" >&2
      continue
    fi

    # docker login: bad credentials fail loud.
    echo "→ Logging in to $host as $user"
    if ! echo "$token" | docker login "$host" -u "$user" --password-stdin >/dev/null 2>&1; then
      echo "⚠  docker login $host failed — credentials may be wrong or expired" >&2
      continue
    fi

    # Persist encrypted in pmcluster so daemon replays on restart.
    if "$PREFIX/pmcluster" registry add "$host" --username "$user" --password-stdin <<<"$token" >/dev/null 2>&1; then
      echo "✅ Registry $host added (user: $user)"
    else
      echo "⚠  pmcluster registry add $host failed (docker login succeeded but encryption/persist failed)" >&2
    fi
  done
}

if [ -n "${PMCLUSTER_REGISTRY:-}" ]; then
  echo "→ Configuring registries from PMCLUSTER_REGISTRY"
  configure_registries "$PMCLUSTER_REGISTRY"
  echo
fi

# --- Install systemd service (Linux only) ---
if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  # Auto-detect the user running this script (or respect PMCLUSTER_USER).
  PMCLUSTER_USER="${PMCLUSTER_USER:-${SUDO_USER:-$(id -un)}}"
  PMCLUSTER_HOME="$(eval echo ~${PMCLUSTER_USER})"

  # Determine the docker group (usually "docker", but some distros use "docker-root").
  DOCKER_GROUP="docker"
  if getent group docker-root >/dev/null 2>&1; then
    DOCKER_GROUP="docker-root"
  fi

  # If we are root (sudo) and the user isn't root, add them to the docker group.
  if [ "$(id -u)" = 0 ] && [ "$PMCLUSTER_USER" != "root" ]; then
    usermod -aG "$DOCKER_GROUP" "$PMCLUSTER_USER" 2>/dev/null || true
  fi

  echo "→ Installing systemd service for user ${PMCLUSTER_USER}"

  # Write the unit file.  Templated at install time so HOME and User are correct.
  sudo tee /etc/systemd/system/pmcluster.service >/dev/null <<UNIT
[Unit]
Description=pmcluster API Server
After=docker.service
Requires=docker.service

[Service]
Type=simple
User=${PMCLUSTER_USER}
Group=${DOCKER_GROUP}
Environment=HOME=${PMCLUSTER_HOME}
ExecStart=${PREFIX}/pmcluster serve
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable pmcluster

  # If pmcluster was already initialized and the cluster is up, start now.
  # Otherwise the operator needs to run 'pmcluster init' + 'pmcluster cluster up'
  # first — we print the commands and let them decide.
  if [ -f "${PMCLUSTER_HOME}/.pmcluster/config.yaml" ]; then
    systemctl start pmcluster
    echo "→ pmcluster started (found existing config at ${PMCLUSTER_HOME}/.pmcluster)"
  else
    echo "→ systemd unit installed but NOT started — run pmcluster init + cluster up first"
  fi

  echo
echo "Next (if not already done):"
echo "  pmcluster init                # create ~/.pmcluster + bootstrap user"
echo "  pmcluster cluster up --domain=<your-domain> --cert=<cert> --key=<key> --openobserve-email=<you@host>"
echo
  echo "Manage the service:"
  echo "  systemctl status pmcluster    # check status"
  echo "  systemctl restart pmcluster   # restart after cluster changes"
  echo "  journalctl -u pmcluster -f    # follow logs"

  if [ -n "${PMCLUSTER_REGISTRY:-}" ]; then
    echo
    echo "Registries were auto-configured. Verify:"
    echo "  pmcluster registry list"
  fi
else
  echo
echo "Next:"
echo "  pmcluster init                # create ~/.pmcluster + bootstrap user"
echo "  pmcluster cluster up --domain=<your-domain> --cert=<cert> --key=<key> --openobserve-email=<you@host>"
echo "  pmcluster serve               # start the daemon (supervise via systemd; sample unit ships in repo)"

  if [ -n "${PMCLUSTER_REGISTRY:-}" ]; then
    echo
    echo "Registries were auto-configured. Verify:"
    echo "  pmcluster registry list"
  fi
fi
