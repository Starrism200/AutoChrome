# AutoChrome

A small toolkit for running Google Chrome on a virtual display (Xvfb) with
self-contained, per-environment profiles and deterministic browser fingerprints.

English | [简体中文](README.zh-CN.md)

## Features

- Install Google Chrome, Xvfb and fonts (`install.sh`)
- Update Chrome/system packages and rebuild (`update.sh`)
- Create self-contained browser environment directories (`env.json` + Chrome profile)
- Generate a deterministic fingerprint per environment (UA, UA-CH, timezone,
  locale, screen, WebGL, CPU and memory)
- Launch headed Chrome through `xvfb-run` when no display is available
- Optional per-environment proxy
- Cross-process browser concurrency limit using `flock` slots
- CDP helpers: screenshots, cookie export, element waits and trusted keyboard events

## Requirements

- Linux
- Go 1.25+ (`GO_BIN` can point at a specific toolchain)
- Google Chrome
- `xvfb`
- `fonts-liberation`

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

## Usage

### Create an environment

```bash
./bin/autochrome gen -id account-01 -root profiles
```

Output:

```text
env_dir=profiles/default-account-01
fingerprint={"user_agent":"...","timezone":"Europe/Paris",...}
```

Layout:

```text
profiles/default-account-01/
├── env.json        # id, platform, creation time and fingerprint
└── ...             # Chrome --user-data-dir contents (created on first launch)
```

### Print a fingerprint

```bash
./bin/autochrome fingerprint -id account-01
```

The same `-id` always produces the same fingerprint.

### Launch the browser

```bash
./bin/autochrome open -id account-01 -url https://example.com
```

- Uses `xvfb-run` automatically when `DISPLAY` is not set (`1366x900x24`)
- Runs headed Chrome inside Xvfb by default
- `-keep` keeps the browser open until Ctrl+C
- `-screenshot out.png` captures a screenshot after launch
- `-proxy socks5://127.0.0.1:10801` binds a proxy
- `-headless` uses Chrome headless mode
- `-chrome /usr/bin/google-chrome` sets the Chrome binary
- `-slot-dir` / `-slot-max` control concurrency slots (or use
  `AUTOCHROME_SLOT_DIR` / `AUTOCHROME_BROWSER_MAX`)

Example:

```bash
./bin/autochrome open \
  -id account-01 \
  -url https://example.com \
  -proxy socks5://127.0.0.1:10801 \
  -screenshot data/shot.png \
  -keep
```

## Layout

```text
cmd/autochrome/        CLI: gen / fingerprint / open
internal/browser/      Chrome launch, CDP operations, screenshots, cookies, UI helpers
internal/browserexec/  Start child processes under DISPLAY or xvfb-run
internal/browserslot/  Cross-process browser concurrency slots
internal/envdir/       Environment directories and metadata
internal/fingerprint/  Deterministic fingerprint generation
internal/ua/           User-agent constants
scripts/common.sh      GO_BIN / CHROME / build / xvfb_run
install.sh             System packages + Chrome + build
update.sh              Upgrade + rebuild
docs/                  Installation and browser notes
```

## Documentation

- [docs/INSTALL.md](docs/INSTALL.md) (Chinese)
- [docs/BROWSER.md](docs/BROWSER.md) (Chinese)
