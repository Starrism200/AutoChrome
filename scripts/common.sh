#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ -z "${GO_BIN:-}" ]; then
  if [ -x /opt/go1.27/bin/go ]; then
    GO_BIN=/opt/go1.27/bin/go
  else
    GO_BIN=go
  fi
fi

CHROME="${CHROME:-/usr/bin/google-chrome}"
ENV_ROOT="${ENV_ROOT:-$ROOT/profiles}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
export GO_BIN CHROME ENV_ROOT BIN_DIR

build() {
  mkdir -p "$BIN_DIR"
  "$GO_BIN" build -o "$BIN_DIR/autochrome" ./cmd/autochrome
}

xvfb_run() {
  if [ -n "${DISPLAY:-}" ]; then
    "$@"
  else
    xvfb-run -a -s "-screen 0 1366x900x24" "$@"
  fi
}
