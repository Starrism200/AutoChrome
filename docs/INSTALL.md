# 安装与更新

## 1. 支持环境

- Debian / Ubuntu（`install.sh` 使用 `apt-get`）
- 需要 root 权限安装系统包
- 已在 Linux + Google Chrome + Xvfb + Go 1.26+ 环境验证

其它发行版可手动安装 Chrome、Xvfb 和字体后直接执行构建：

```bash
source scripts/common.sh
build
```

## 2. 依赖清单

| 依赖 | 用途 | 安装来源 |
|---|---|---|
| Google Chrome | 被自动化的浏览器 | `dl.google.com` 的 deb 包 |
| `xvfb` | 虚拟显示器（无物理屏幕时有头运行 Chrome） | apt |
| `x11vnc` | 把 Xvfb 暴露为 VNC，供人工接管 | apt |
| `novnc` / `websockify` | 浏览器版 VNC 客户端 | apt |
| `fonts-liberation` | 基础字体，避免页面渲染异常 | apt |
| `ca-certificates` / `curl` | 下载 Chrome | apt |
| `dpkg-deb` | 安装托管 Chrome `.deb` 版本 | apt / dpkg |
| Go 1.26+ | 编译 | 手动安装或用 `GO_BIN` 指定 |

## 3. 首次安装

```bash
cd AutoChrome
sudo ./install.sh
```

脚本行为：

1. `apt-get update`
2. 缺少时安装 `xvfb`、`fonts-liberation`、`ca-certificates`、`curl`、
   `x11vnc`、`novnc`、`websockify`
3. 缺少 `/usr/bin/google-chrome` 时下载并安装 Chrome 稳定版
4. 检查 Go 工具链
5. 编译 `bin/autochrome`
6. 打印 Chrome 版本

`GO_BIN` 选择顺序：

1. 环境变量 `GO_BIN`
2. `PATH` 中版本满足 1.25+ 的 `go`
3. `/usr/local/go/bin/go` 或 `/opt/go*/bin/go`（版本满足时）

也可以在执行脚本前显式指定：

```bash
GO_BIN=/usr/local/go/bin/go ./install.sh
```

## 4. 更新

```bash
sudo ./update.sh
```

- 升级 `xvfb`、`fonts-liberation`、`ca-certificates`
- 重新下载并安装最新 Chrome 稳定版
- 重新编译 `bin/autochrome`

只重新编译：

```bash
SKIP_SYSTEM=1 ./update.sh
```

## 5. Chrome 多版本

托管版本默认放在 `/opt/autochrome/chrome`，可用 `AUTOCHROME_CHROME_DIR` 覆盖。

```bash
TOKEN=$(cat data/users/default/api_token)
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8787/api/v1/chrome/update
```

也可以上传本地 `.deb`：

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -F 'file=@google-chrome-stable_current_amd64.deb' \
  http://127.0.0.1:8787/api/v1/chrome/install
```

旧版本需要自行准备对应的 `.deb` 再通过 `/api/v1/chrome/install` 安装。

## 6. 验证安装

```bash
google-chrome --version
xvfb-run -a google-chrome --version
./bin/autochrome gen -id test
./bin/autochrome open -id test -url https://example.com -screenshot /tmp/test.png
```

若 `open` 能生成截图，说明浏览器与虚拟显示器都正常。

## 7. 常见问题

### `xvfb-run: command not found`

```bash
sudo apt-get install -y xvfb
```

### `Failed to move to new namespace` / Chrome 启动失败

以 root 运行时 Chrome 需要 `--no-sandbox`。`internal/browser` 已默认开启
`no-sandbox` 和 `disable-setuid-sandbox`。

### `Missing X server or $DISPLAY`

`open` 在没有 `DISPLAY` 时会自动调用 `xvfb-run`。若你手动设置了 `DISPLAY`
但该显示不可用，请先 `unset DISPLAY`。

### 字体方块 / 中文乱码

```bash
sudo apt-get install -y fonts-liberation fonts-noto-cjk
```

### Chrome 升级后指纹不一致

Chrome 大版本会改变 UA、UA-CH 和部分渲染特征。升级后请重新生成环境或确认
`internal/ua/ua.go` 中的 `ChromeMajor` 与已安装 Chrome 一致。

### apt 锁被占用

等待其它 apt 进程结束，或删除 `/var/lib/dpkg/lock-frontend` 对应的进程后重试。
