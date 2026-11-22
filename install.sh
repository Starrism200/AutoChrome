#!/usr/bin/env bash
# Install Google Chrome, Xvfb, fonts and the build dependencies, then build
# the autochrome binary.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"
source "$ROOT/scripts/common.sh"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root: sudo ./install.sh" >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive

echo "==> installing system packages"
apt-get update

packages=()
command -v xvfb-run >/dev/null 2>&1 || packages+=(xvfb)
command -v x11vnc >/dev/null 2>&1 || packages+=(x11vnc)
command -v websockify >/dev/null 2>&1 || packages+=(websockify)
[ -d /usr/share/novnc ] || packages+=(novnc)
command -v curl >/dev/null 2>&1 || packages+=(curl)
command -v dpkg-deb >/dev/null 2>&1 || packages+=(dpkg)
dpkg -s fonts-liberation >/dev/null 2>&1 || packages+=(fonts-liberation)
dpkg -s ca-certificates >/dev/null 2>&1 || packages+=(ca-certificates)
if [ "${#packages[@]}" -gt 0 ]; then
  apt-get install -y "${packages[@]}"
fi

if ! command -v google-chrome >/dev/null 2>&1 && [ ! -x /usr/bin/google-chrome ]; then
  echo "==> installing Google Chrome"
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

mkdir -p /opt/autochrome/chrome

echo "==> building $BIN_DIR/autochrome"
build

echo "==> installed versions"
google-chrome --version
"$BIN_DIR/autochrome" help >/dev/null 2>&1 || true
echo "install ok"
