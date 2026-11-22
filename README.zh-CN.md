# AutoChrome

[English](README.md) | 简体中文

浏览器 + 虚拟显示器（Xvfb）工具集：为每个环境生成独立的 Chrome profile 和
确定性指纹，并在没有物理显示器时通过 Xvfb 启动有头 Chrome。

## 功能

- 一键安装 Google Chrome、Xvfb、字体等系统依赖（`install.sh`）
- 一键升级 Chrome / 系统依赖并重新编译（`update.sh`）
- 为每个环境生成独立、可归档的浏览器目录（`env.json` + Chrome profile）
- 基于种子确定性生成浏览器指纹（UA、UA-CH、时区、语言、屏幕、WebGL、CPU/内存）
- 在没有物理显示器时自动用 `xvfb-run` 启动有头 Chrome
- 支持按环境绑定代理
- 跨进程浏览器并发上限（`flock` 槽位）
- 提供 CDP 常用能力：截图、Cookie 导出、等待元素、真实键盘事件等

## 依赖

- Linux
- Go 1.25+（可用 `GO_BIN` 指定工具链）
- Google Chrome
- `xvfb`
- `fonts-liberation`

## 安装

```bash
sudo ./install.sh
```

安装脚本会在缺少时安装 `xvfb`、`fonts-liberation`、`ca-certificates`、`curl`，
安装 Google Chrome 稳定版，并编译 `bin/autochrome`。

## 更新

```bash
sudo ./update.sh          # 升级系统依赖 + Chrome，并重新编译
SKIP_SYSTEM=1 ./update.sh # 只重新编译
```

## 构建

```bash
source scripts/common.sh
build

# 或
go build -o bin/autochrome ./cmd/autochrome
```

## 使用

### 生成浏览器环境

```bash
./bin/autochrome gen -id account-01 -root profiles
```

输出：

```text
env_dir=profiles/default-account-01
fingerprint={"user_agent":"...","timezone":"Europe/Paris",...}
```

目录结构：

```text
profiles/default-account-01/
├── env.json        # id、platform、创建时间、指纹
└── ...             # Chrome --user-data-dir 内容（首次启动后生成）
```

### 查看指纹

```bash
./bin/autochrome fingerprint -id account-01
```

同一 `-id` 始终得到同一份指纹。

### 启动浏览器（虚拟显示器）

```bash
./bin/autochrome open -id account-01 -url https://example.com
```

- 没有 `DISPLAY` 时会自动用 `xvfb-run` 包裹自己，屏幕 `1366x900x24`
- 默认在 Xvfb 中运行**有头** Chrome
- `-keep` 保持浏览器打开，按 Ctrl+C 关闭
- `-screenshot out.png` 启动后截图
- `-proxy socks5://127.0.0.1:10801` 绑定代理
- `-headless` 使用 Chrome headless 模式
- `-chrome /usr/bin/google-chrome` 指定 Chrome 路径
- `-slot-dir` / `-slot-max` 控制并发槽位（也可用环境变量
  `AUTOCHROME_SLOT_DIR` / `AUTOCHROME_BROWSER_MAX`）

示例：

```bash
./bin/autochrome open \
  -id account-01 \
  -url https://example.com \
  -proxy socks5://127.0.0.1:10801 \
  -screenshot data/shot.png \
  -keep
```

## 目录结构

```text
cmd/autochrome/        命令行入口：gen / fingerprint / open
internal/browser/      Chrome 启动、CDP 操作、截图、Cookie、UI 辅助
internal/browserexec/  在 DISPLAY 或 xvfb-run 下启动子进程
internal/browserslot/  跨进程浏览器并发槽位
internal/envdir/       环境目录与元数据
internal/fingerprint/  确定性浏览器指纹生成
internal/ua/           UA 常量
scripts/common.sh      GO_BIN / CHROME / 构建 / xvfb_run
install.sh             系统依赖 + Chrome + 构建
update.sh              升级 + 重新构建
docs/                  安装与浏览器技术说明
```

## 文档

- [docs/INSTALL.md](docs/INSTALL.md)：安装、更新、依赖排查
- [docs/BROWSER.md](docs/BROWSER.md)：虚拟显示器、指纹、启动参数、并发与代理
