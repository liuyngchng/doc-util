#!/usr/bin/env bash
# Ensure Go SDK tarball is cached locally (dist/cache/deps/go<VERSION>.linux-amd64.tar.gz).
# download if missing. Called by build_cli.sh / build.sh / build_mcp.sh / dev.sh.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.24.13}"
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="$SCRIPT_DIR/dist/cache/deps"
TARBALL="$OUT_DIR/go${GO_VERSION}.linux-amd64.tar.gz"
URL="https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"

if [[ -f "$TARBALL" ]]; then
  echo "Go SDK tarball ready: $TARBALL"
  exit 0
fi

mkdir -p "$OUT_DIR"
echo "Downloading Go ${GO_VERSION} toolchain ..."

if command -v curl &>/dev/null; then
  curl -fL --retry 3 -o "$TARBALL" "$URL"
elif command -v wget &>/dev/null; then
  wget -O "$TARBALL" "$URL"
else
  echo "ERROR: neither curl nor wget found; cannot download Go toolchain" >&2
  exit 1
fi

echo "Go SDK tarball saved to $TARBALL"