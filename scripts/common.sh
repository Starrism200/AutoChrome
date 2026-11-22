#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go_ok() {
  "$1" version 2>/dev/null | grep -qE 'go1\.(2[6-9]|[3-9][0-9])'
}

if [ -z "${GO_BIN:-}" ]; then
  GO_BIN=go
  if ! command -v "$GO_BIN" >/dev/null 2>&1 || ! go_ok "$GO_BIN"; then
    for candidate in /usr/local/go/bin/go /opt/go*/bin/go; do
      if [ -x "$candidate" ] && go_ok "$candidate"; then
        GO_BIN="$candidate"
        break
      fi
    done
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
    xvfb-run -a -s "-screen 0 1920x1080x24" "$@"
  fi
}
