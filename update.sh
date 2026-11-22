#!/usr/bin/env bash
# Update system packages (Xvfb/fonts), upgrade Google Chrome to the latest
# stable build and rebuild the autochrome binary.
#
# Usage:
#   sudo ./update.sh              # update system packages + Chrome + rebuild
#   SKIP_SYSTEM=1 ./update.sh     # rebuild only
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"
source "$ROOT/scripts/common.sh"

if [ "${SKIP_SYSTEM:-0}" != "1" ]; then
  if [ "$(id -u)" -ne 0 ]; then
    echo "run as root for system updates: sudo ./update.sh" >&2
    echo "or rebuild only: SKIP_SYSTEM=1 ./update.sh" >&2
    exit 1
  fi
  export DEBIAN_FRONTEND=noninteractive

  echo "==> updating system packages"
  apt-get update
  apt-get install -y --only-upgrade xvfb fonts-liberation ca-certificates x11vnc novnc websockify || true

  echo "==> upgrading Google Chrome"
  tmp="$(mktemp -d)"
  curl -fsSL -o "$tmp/google-chrome.deb" \
    https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
  apt-get install -y "$tmp/google-chrome.deb"
  rm -rf "$tmp"
fi

if ! command -v "$GO_BIN" >/dev/null 2>&1; then
  echo "Go toolchain not found: $GO_BIN" >&2
  echo "Install Go 1.26+ or set GO_BIN=/path/to/go" >&2
  exit 1
fi

echo "==> rebuilding $BIN_DIR/autochrome"
build

echo "==> current versions"
google-chrome --version
echo "update ok"
