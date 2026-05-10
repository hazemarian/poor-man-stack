#!/usr/bin/env bash
# install.sh — fetch the latest pmcluster release binary and drop it in
# /usr/local/bin (override with PREFIX=/path/to/dir).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-stack/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-stack/main/install.sh | VERSION=v0.2.0 bash

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
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
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
echo
"$PREFIX/pmcluster" version || true
echo
echo "Next:"
echo "  pmcluster init                # create ~/.pmcluster + bootstrap user"
echo "  pmcluster cluster up --domain=<your-domain> --cert=<cert> --key=<key> --openobserve-email=<you@host>"
echo "  pmcluster serve               # start the daemon (supervise via systemd; sample unit ships in repo)"
