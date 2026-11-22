# AutoChrome

A small toolkit for running Google Chrome on a virtual display (Xvfb) with
self-contained, per-environment profiles and deterministic browser fingerprints.

It also ships a synchronous JavaScript SDK and a Model Context Protocol (MCP)
server so AI clients can drive Chrome or run generated scripts.

English | [简体中文](README.zh-CN.md)

## Features

- Install Google Chrome, Xvfb and fonts (`install.sh`)
- Update Chrome/system packages and rebuild (`update.sh`)
- Containers/environments with unique name + id and an isolated directory
- Create, update, inspect, delete, import and export containers
- Generate a deterministic fingerprint per environment (UA, UA-CH, timezone,
  locale, screen, WebGL, CPU and memory)
- Launch headed Chrome through `xvfb-run` when no display is available
- Optional per-environment proxy
- Cross-process browser concurrency limit using `flock` slots
- Embedded JavaScript script SDK (`ac` / `page`) without a Node.js dependency
- MCP stdio server with browser tools and resources
- HTTP/JSON API (`autochrome serve`) with Bearer authentication
- Proxy registry with per-environment binding and reachability checks
- Configurable environment/attachment roots with generic file upload/download
- Environment export/import archives (`.tar.gz`)
- Side-by-side Chrome version management (install/use/update/remove)
- Public noVNC tokens proxied by the API with 300s idle disconnect
- SSE events, batch session open, token rotation and interval schedules
- OpenAPI document at `/api/v1/openapi.yaml`
- Cookie + user-agent export/import
- Short-lived interaction page for QR / OTP / CAPTCHA handoff
- VNC/noVNC desktop for human takeover (QR scan, SMS code, CAPTCHA)
- Challenge detection, QR capture and coordinate clicks
- Multi-user isolation (L2: one MCP process and one directory tree per user)
- CDP helpers: snapshots with element refs, screenshots, cookies, trusted keys

## Architecture

One Chrome installation (the system Chrome or a managed version) is shared.
Every container (environment) has a unique name/id and its own directory; each
live session runs a separate Chrome process. Users are isolated by process and directory (L2), not
by OS users or containers.

## Requirements

- Linux
- Go 1.26+ (`GO_BIN` can point at a specific toolchain)
- Google Chrome
- `xvfb`
- `fonts-liberation`
- `x11vnc`, `novnc`, `websockify` (for VNC; installed by `install.sh`)
- `dpkg-deb` (for installing managed Chrome `.deb` builds)

## Install

```bash
sudo ./install.sh
```

The script installs `xvfb`, `fonts-liberation`, `ca-certificates` and `curl`
when missing, installs Google Chrome stable when missing, and builds
`bin/autochrome`.

## Update

```bash
sudo ./update.sh          # upgrade system packages + Chrome, then rebuild
SKIP_SYSTEM=1 ./update.sh # rebuild only
```

## Build

```bash
source scripts/common.sh
build

# or
go build -o bin/autochrome ./cmd/autochrome
```

## Quick start

```bash
sudo ./install.sh

./bin/autochrome gen -id account-01
./bin/autochrome open -id account-01 -url https://example.com
```

`gen` creates `profiles/default-account-01/` with an `env.json` fingerprint.
`open` launches headed Chrome on Xvfb and can save a screenshot:

```bash
./bin/autochrome open -id account-01 -url https://example.com \
  -screenshot shot.png -keep
```

## Users

The `default` user keeps the historical layout. Other users live under
`data/users/<id>/` with their own profiles, scripts, screenshots, logs and
browser slots.

```bash
./bin/autochrome user add alice --allow-eval --allow-scripts --max-browsers 2
./bin/autochrome user list
./bin/autochrome user show alice

./bin/autochrome gen -user alice -id account-01
```

Resolution order: `-user` > `AUTOCHROME_USER` > `default`.
See [docs/MULTIUSER.md](docs/MULTIUSER.md).

## JavaScript scripts

Scripts are synchronous JavaScript run in an embedded runtime. They receive
`input`, `ac` and `console`, and return JSON-serializable values.

```bash
./bin/autochrome script list
./bin/autochrome script run example -input '{"url":"https://example.com"}'
```

Example `scripts/custom/example.js`:

```js
const page = ac.open({
  env: input.env || "example",
  url: input.url || "https://example.com",
});

const snap = page.snapshot({ max: 50 });
const title = page.title();
page.screenshot({ path: "example.png", fullPage: true });
page.close();

return { title, nodes: snap.nodes.length };
```

Full API and an AI prompt template: [docs/SCRIPT_SDK.md](docs/SCRIPT_SDK.md).

## MCP server

```bash
./bin/autochrome mcp -user alice
```

Client configuration:

```json
{
  "mcpServers": {
    "autochrome": {
      "command": "/path/to/AutoChrome/bin/autochrome",
      "args": ["mcp", "-user", "alice"],
      "env": {
        "AUTOCHROME_HOME": "/path/to/AutoChrome"
      }
    }
  }
}
```

The server exposes environment, session, navigation, snapshot, click, type,
wait, screenshot, cookie, eval and script tools, plus `autochrome://envs` and
`autochrome://sessions` resources.
See [docs/MCP.md](docs/MCP.md).

## HTTP API

```bash
./bin/autochrome serve -user alice -addr 127.0.0.1:8787
```

The server uses a per-user token stored at `data/users/alice/api_token` and
exposes containers/environments, sessions, navigation, snapshots, screenshots, challenges,
scripts, proxies and VNC information under `/api/v1`. It also supports proxy
CRUD/checks, per-environment proxy binding, storage roots, attachments,
environment export/import, cookie+UA export/import, Chrome version management,
SSE events, batch opens, token rotation, interval schedules and a short-lived
interaction page generated by `GET /api/v1/sessions/{id}/interaction`.
OpenAPI: `GET /api/v1/openapi.yaml`.

```bash
TOKEN=$(cat data/users/alice/api_token)
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8787/api/v1/sessions
```

See [docs/API.md](docs/API.md).

## VNC / human takeover

```bash
./bin/autochrome mcp -user alice --vnc
```

For public access, call `POST /api/v1/desktop/token` to get a short-lived
`/vnc/<token>/...` URL proxied by the API. Tokens disconnect after 300 seconds
of inactivity. The local noVNC URL is still printed on stderr and exposed via
`desktop_info` / `autochrome://desktop`.

Ports are auto-assigned from the user's configured range, default
`10000-59999`:

```bash
./bin/autochrome user add alice --vnc-port-start 20000 --vnc-port-end 20999
```

For a separate service:

```bash
./bin/autochrome desktop -user alice
DISPLAY=:100 ./bin/autochrome mcp -user alice
```

See [docs/VNC.md](docs/VNC.md) and [docs/INTERACTION.md](docs/INTERACTION.md).

## Layout

```text
cmd/autochrome/        CLI: gen / open / user / script / mcp
internal/browser/      Chrome launch, CDP operations, snapshots, screenshots
internal/browserslot/  Cross-process browser concurrency slots
internal/envdir/       Environment directories, registry and archives
internal/attachments/  Generic attachment store
internal/fingerprint/  Deterministic fingerprint generation
internal/users/        Per-user directories, quotas and permissions
internal/session/      Long-lived browser sessions
internal/jsruntime/    Embedded JavaScript SDK (goja)
internal/scripts/      Script discovery and manifests
internal/mcpserver/    MCP stdio server
internal/apiserver/    HTTP/JSON API server
internal/chrome/       Managed Chrome versions
internal/scheduler/    Interval schedules
internal/filelock/     Cross-process index locks
internal/atomicfile/   Atomic file writes
internal/xvfb/         Shared virtual display
internal/desktop/      VNC/noVNC desktop service
internal/ua/           User-agent constants
scripts/common.sh      GO_BIN / CHROME / build / xvfb_run
scripts/custom/        Example user scripts
install.sh             System packages + Chrome + build
update.sh              Upgrade + rebuild
docs/                  Installation, browser, SDK, MCP and multi-user notes
```

## Documentation

- [docs/INSTALL.md](docs/INSTALL.md) (Chinese)
- [docs/BROWSER.md](docs/BROWSER.md) (Chinese)
- [docs/SCRIPT_SDK.md](docs/SCRIPT_SDK.md) (Chinese)
- [docs/MCP.md](docs/MCP.md) (Chinese)
- [docs/MULTIUSER.md](docs/MULTIUSER.md) (Chinese)
- [docs/VNC.md](docs/VNC.md) (Chinese)
- [docs/INTERACTION.md](docs/INTERACTION.md) (Chinese)
- [docs/API.md](docs/API.md) (Chinese)
