# 浏览器与虚拟显示器

## 1. 总体结构

```text
cmd/autochrome
  ├─ gen          envdir.Create + fingerprint.Generate
  ├─ fingerprint  fingerprint.Generate
  └─ open         xvfb 包裹 → browser.Launch → CDP
                        │
        ┌───────────────┼────────────────┐
        ▼               ▼                ▼
  browserslot     browser.Launch    fingerprint
  （并发槽位）    （Chrome 启动）    （身份生成）
```

- `internal/envdir`：每个环境一个自包含目录，写入 `env.json`
- `internal/fingerprint`：用环境 id 作为种子，确定性生成身份
- `internal/browser`：组装 Chrome 启动参数、CDP 操作与页面辅助函数
- `internal/browserslot`：用 `flock` 跨进程限制同时运行的浏览器数量

## 2. 虚拟显示器（Xvfb）

`autochrome open` 启动时：

1. 如果 `DISPLAY` 已存在，直接在当前显示上运行
2. 否则用 `xvfb-run -a -s "-screen 0 1920x1080x24"` 重新执行自己
3. `xvfb-run` 为子进程导出 `DISPLAY`，子进程不再递归

这样做的原因：纯 `--headless` 的浏览器特征与真实有头浏览器不同，部分站点更
容易识别自动化。在 Xvfb 中运行**有头** Chrome 可以兼顾无物理屏幕和真实性。

## 3. 环境目录

```bash
./bin/autochrome gen -id account-01 -root profiles
```

生成：

```text
profiles/default-account-01/
└── env.json
```

`env.json`：

```json
{
  "id": "account-01",
  "platform": "default",
  "dir": "/abs/path/profiles/default-account-01",
  "created_at": "2026-11-22T10:00:00Z",
  "fingerprint": {
    "user_agent": "...",
    "platform": "Win32",
    "language": "en-US",
    "accept_language": "en-US,en",
    "timezone": "America/New_York",
    "width": 1920,
    "height": 1080,
    "pixel_ratio": 1,
    "hardware_concurrency": 8,
    "device_memory": 8,
    "webgl_vendor": "Google Inc. (NVIDIA)",
    "webgl_renderer": "ANGLE (...)"
  }
}
```

目录名规则：`<platform>-<sanitized id>`。`id` 中不安全字符会被替换为 `_`，
最长 80 字符。

环境目录是 Chrome 的 `--user-data-dir`，包含 Cookie、localStorage 等浏览器状态。

## 4. 指纹生成

`fingerprint.Generate(seed)` 用 FNV-1a + LCG 从固定集合中选择：

- 操作系统 / UA（Windows、macOS、Linux）
- WebGL vendor / renderer
- 语言与 `Accept-Language`
- 时区
- 屏幕分辨率与 `devicePixelRatio`
- CPU 核心数、设备内存

同一个 seed 永远得到同一份指纹。`open` 会优先读取 `env.json` 中的指纹，
不存在时才重新生成，因此环境迁移后身份不漂移。

启动后通过 CDP 应用：

- `Emulation.setTimezoneOverride`
- `Emulation.setDeviceMetricsOverride`
- `Emulation.setUserAgentOverride`（含 UA-CH）
- `Page.addScriptToEvaluateOnNewDocument` 覆盖
  `navigator.hardwareConcurrency`、`navigator.deviceMemory`、WebGL
  `getParameter(37445/37446)`

> 注意：这些是应用层伪装，无法改变真实的 TLS/JA3、GPU、字体、音频等底层特征。

## 5. Chrome 启动参数

`browser.Launch` 默认参数：

| 参数 | 作用 |
|---|---|
| `--user-data-dir` | 每环境独立 profile |
| `--no-sandbox` / `--disable-setuid-sandbox` | root 环境可启动 |
| `--disable-dev-shm-usage` | 容器内避免 `/dev/shm` 不足 |
| `--disable-gpu` | 无 GPU 环境稳定运行 |
| `--hide-scrollbars` | 截图更干净 |
| `--mute-audio` | 静音 |
| `--disable-blink-features=AutomationControlled` | 减少自动化特征 |
| `--disable-quic` | 统一走代理 TCP |
| `--force-webrtc-ip-handling-policy=disable_non_proxied_udp` | 禁止 WebRTC 绕过代理 |
| `--deny-permission-prompts` | 拒绝权限弹窗 |
| `--lang` / `--accept-lang` | 与指纹一致 |
| `--disk-cache-size` / `--media-cache-size` | 限制缓存体积 |
| `--aggressive-cache-discard` | 及时清理缓存 |
| `--disable-component-update` 等 | 关闭后台组件/崩溃上报/同步 |
| `--proxy-server` | 可选，绑定代理 |
| `--headless=new` | 仅在 `-headless` 时启用 |

`/dev/shm/autochrome-cache/<env>` 用作临时磁盘缓存，退出时删除。

启动前会清理 `SingletonLock`、`SingletonSocket`、`SingletonCookie`，避免
异常退出后 Chrome 无法附着 CDP。

## 6. 并发控制

`internal/browserslot` 使用 `flock` 在一组槽位文件上加排他锁：

- 默认目录 `data/browser-slots`
- 默认上限 10
- 环境变量：`AUTOCHROME_SLOT_DIR`、`AUTOCHROME_BROWSER_MAX`
- 命令行：`-slot-dir`、`-slot-max`
- 等待超时默认 10 分钟

因为锁是文件级的，多个进程可以共享同一个上限。

## 7. 代理

`-proxy` 直接传给 Chrome 的 `--proxy-server`，常见格式：

```bash
-proxy socks5://127.0.0.1:10801
-proxy http://127.0.0.1:8080
-proxy socks5h://127.0.0.1:10801
```

配合 `--disable-quic` 和 WebRTC 策略，可以避免 UDP/直连绕过代理。

## 8. CDP 辅助函数

`internal/browser` 提供：

- `Launch` / `emulateDesktop`
- `Screenshot`
- `SaveCookiesFor`
- `EvalString` / `WaitString` / `WaitSelector` / `WaitEnabled` / `WaitButtonText`
- `Activate` / `ActivateByText` / `ClickByText`（聚焦 + 真实 Enter 键）
- `Fill`（Click + SendKeys）
- `DismissCookieBanner`（多语言 Cookie 同意弹窗）

真实键盘事件（`isTrusted=true`）比 JS `element.click()` 更接近人工操作。

## 9. 验证

```bash
# 1. 生成环境
./bin/autochrome gen -id test

# 2. 无显示器启动并截图
./bin/autochrome open -id test -url https://example.com -screenshot /tmp/a.png

# 3. 查看环境元数据
cat profiles/default-test/env.json

# 4. 验证代理出口（示例）
curl -x socks5h://127.0.0.1:10801 https://ipinfo.io/json
```
