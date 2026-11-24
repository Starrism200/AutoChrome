# HTTP API

`autochrome serve` 提供一个本地 HTTP/JSON API，适合前端、脚本或其它后端调用。
每个进程只服务一个用户，和 MCP 的 L2 隔离模型一致。

## 1. 启动

```bash
./bin/autochrome serve -user alice -addr 127.0.0.1:8787
```

可选：

```bash
--token TOKEN          # 指定 token（也可用 AUTOCHROME_API_TOKEN）
--cors http://localhost:3000
--vnc                  # 同时启动 x11vnc + noVNC
--mcp                  # 在同一端口挂载 MCP Streamable HTTP /mcp
--allow-eval           # 强制开启 /eval
--allow-scripts        # 强制开启脚本运行
```

公网访问（尤其是 VNC token）时使用 `--addr 0.0.0.0:8787` 或放在 TLS
反向代理后面。

首次启动会生成 token 并写入：

```text
data/users/<user>/api_token    # 0600
```

使用已有 token 时只打印路径；新生成时会打印一次 token。

## 2. 认证

所有接口（除 `/api/v1/health`）都需要：

```http
Authorization: Bearer <token>
```

也支持：

```http
X-API-Token: <token>
```

示例：

```bash
TOKEN=$(cat data/users/default/api_token)
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/api/v1/user
```

## 3. 错误格式

```json
{
  "error": {
    "code": "session_not_found",
    "message": "session \"...\" not found"
  }
}
```

常见状态码：`400` 参数错误、`401` 未认证、`403` 权限关闭、`404` 不存在、
`502` 浏览器操作失败、`504` 等待超时。

## 4. 接口

### 基础

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/health` | 健康检查（无需认证） |
| GET | `/api/v1/openapi.yaml` | OpenAPI 文档（无需认证） |
| GET | `/api/v1/user` | 当前用户与配置 |
| POST | `/api/v1/token/rotate` | 轮换 API token，返回新 token |
| GET | `/api/v1/events` | SSE 事件流 |

### 存储

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/storage` | 当前环境/附件/脚本/截图根目录 |
| PUT | `/api/v1/storage` | 修改 `{env_root?, attachment_root?}` |

`env_root` / `attachment_root` 可以是绝对路径，也可以相对项目根目录。
修改 `env_root` 后，旧环境通过 `envs.json` 索引仍然可以查询和打开；新环境使用
新目录。附件索引保存绝对路径，旧附件不受影响。

### 环境

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/envs` | 环境列表 |
| POST | `/api/v1/envs` | 创建环境 `{id, platform?}` |
| GET | `/api/v1/envs/{id}` | 环境详情 |
| PATCH | `/api/v1/envs/{id}` | 修改容器 `{name?, description?, tags?}` |
| PUT | `/api/v1/envs/{id}` | 同 PATCH |
| DELETE | `/api/v1/envs/{id}` | 删除环境（会先关闭该环境的会话） |
| PUT | `/api/v1/envs/{id}/proxy` | 绑定代理 `{proxy_id}` |
| DELETE | `/api/v1/envs/{id}/proxy` | 解绑代理 |

### 容器别名

“容器”就是环境实例，每个容器有唯一 `id`/`name` 和独立 `dir`。以下接口
`{name}` 可以传唯一名字或 id：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/containers` | 容器列表 |
| POST | `/api/v1/containers` | 新增容器 `{id, name?, description?, tags?, platform?, chrome_version?}` |
| GET | `/api/v1/containers/{name}` | 查询容器 |
| PATCH | `/api/v1/containers/{name}` | 修改容器 |
| PUT | `/api/v1/containers/{name}` | 同 PATCH |
| DELETE | `/api/v1/containers/{name}` | 删除容器 |
| GET | `/api/v1/containers/{name}/export` | 导出容器 |
| POST | `/api/v1/containers/import` | 导入容器 |
| PUT | `/api/v1/containers/{name}/proxy` | 绑定代理 |
| DELETE | `/api/v1/containers/{name}/proxy` | 解绑代理 |
| PUT | `/api/v1/containers/{name}/chrome` | 绑定 Chrome 版本 |
| DELETE | `/api/v1/containers/{name}/chrome` | 解绑 Chrome 版本 |
| PUT | `/api/v1/containers/{name}/size` | 设置浏览器视口，默认重启会话 |
| PATCH | `/api/v1/containers/{name}/fingerprint` | 修改语言/时区/UA/屏幕等指纹，默认重启会话 |
| PUT | `/api/v1/containers/{name}/fingerprint` | 同 PATCH |
| POST | `/api/v1/containers/{name}/restart` | 重新拉起容器浏览器 |

`id` 创建后不可变；`name` 可修改且在同一用户下唯一。目录名由 `id` 决定，
每个容器独立，不会因为改名而移动目录。

关闭 VNC 里最后一个标签页后，不需要重启服务或容器，调用：

```bash
curl -X POST -H "$H" "$BASE/containers/demo/restart"
```

容器尺寸接口：

```bash
curl -X PUT -H "$H" -H 'Content-Type: application/json' \
  -d '{"width":1920,"height":1080,"restart":true}' \
  "$BASE/containers/demo/size"
```

默认浏览器视口是 `1366x900`，可设置为 `1920x1080`。**noVNC 画布大小仍是
该用户进程的 Xvfb 屏幕**，默认 `1920x1080`，启动时可用
`--vnc-screen 1366x900x24` 覆盖；当前不是每容器独立画布。

容器指纹接口：

```bash
curl -X PATCH -H "$H" -H 'Content-Type: application/json' \
  -d '{
    "language": "fr-FR",
    "accept_language": "fr-FR,fr,en",
    "timezone": "Europe/Paris",
    "width": 1920,
    "height": 1080,
    "color_depth": 24,
    "pixel_ratio": 1,
    "hardware_concurrency": 8,
    "device_memory": 8,
    "webgl_vendor": "Google Inc. (Intel)",
    "webgl_renderer": "ANGLE (Intel, Mesa Intel(R) UHD Graphics 620 (KBL GT2), OpenGL 4.6)",
    "restart": true
  }' \
  "$BASE/containers/demo/fingerprint"
```

所有字段可选，未传字段保持当前值。修改后默认关闭并重启现有会话；没有会话时
只更新配置，下一次打开生效。`restart:false` 可只更新配置。

当前指纹应用方式：

- 语言：Chrome `--lang`、`--accept-lang`，并通过 CDP
  `Emulation.setUserAgentOverride` 和页面脚本设置 `navigator.language/languages`
- 时区：CDP `Emulation.setTimezoneOverride`
- 屏幕：`--window-size` + CDP `Emulation.setDeviceMetricsOverride`
- UA/平台/UA-CH：`--user-agent` + CDP `Emulation.setUserAgentOverride`，UA-CH
  的品牌版本从 UA 里的 Chrome 主版本推导
- `hardwareConcurrency`、`deviceMemory`、WebGL vendor/renderer、screen
  `colorDepth/pixelDepth`：新文档注入脚本覆盖

注意：Canvas、Audio、字体列表、TLS/JA3、GPU 驱动层没有做完整伪造；WebGL
只是字符串级覆盖。时区必须是 IANA 名称，例如 `Asia/Shanghai`。

### 环境打包

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/envs/{id}/export` | 下载 `.tar.gz`（含 profile 和 `env.json`） |
| POST | `/api/v1/envs/import` | multipart `file` 导入 `.tar.gz` |

导出前可加 `?close=1` 先关闭该环境的会话。导入会拒绝路径穿越和已存在的 ID。

### 代理

代理保存在 `data/users/<id>/proxies.json`，支持 `http`、`https`、`socks`、
`socks5`、`socks5h` 或裸 `host:port`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/proxies` | 代理列表 |
| POST | `/api/v1/proxies` | 新增 `{id?, name?, url, enabled?, tags?}` |
| GET | `/api/v1/proxies/{id}` | 代理详情 |
| PATCH | `/api/v1/proxies/{id}` | 修改 `{name?, url?, enabled?, tags?}` |
| PUT | `/api/v1/proxies/{id}` | 同 PATCH |
| DELETE | `/api/v1/proxies/{id}` | 删除 |
| POST | `/api/v1/proxies/{id}/check` | TCP 连通性检测，回写 `last_check_*` |

绑定后，打开该环境的会话时会自动使用绑定的代理；会话请求里的 `proxy` 或
`proxy_id` 优先级更高。

### 会话

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/sessions` | 存活会话列表 |
| POST | `/api/v1/sessions` | 打开会话 `{env, platform?, url?, proxy?, proxy_id?, chrome_version?, headless?}` |
| POST | `/api/v1/sessions/batch` | 批量打开 `{envs, url?, proxy_id?, chrome_version?, headless?}` |
| DELETE | `/api/v1/sessions` | 关闭所有会话 |
| GET | `/api/v1/sessions/{id}` | 会话信息 |
| DELETE | `/api/v1/sessions/{id}` | 关闭会话 |
| POST | `/api/v1/sessions/{id}/restart` | 重新拉起浏览器 `{url?}`，不需要重启服务 |

### 页面

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/sessions/{id}/navigate` | `{url}` |
| POST | `/api/v1/sessions/{id}/reload` | 刷新当前页面 |
| POST | `/api/v1/sessions/{id}/back` | 后退 |
| GET | `/api/v1/sessions/{id}/snapshot?max=300` | 带 `e1` 引用的文本快照 |
| POST | `/api/v1/sessions/{id}/click` | `{ref?, selector?, text?}` |
| POST | `/api/v1/sessions/{id}/type` | `{ref?, selector?, text, submit?}` |
| POST | `/api/v1/sessions/{id}/press` | `{key}` |
| POST | `/api/v1/sessions/{id}/wait` | `{selector?, hidden_selector?, text?, url_contains?, cookie_name?, timeout_ms?}` |
| POST | `/api/v1/sessions/{id}/eval` | `{expression}`，需 `allow_eval` |
| GET | `/api/v1/sessions/{id}/screenshot` | 返回 `image/png` |
| GET | `/api/v1/sessions/{id}/screenshot?json=1&name=a.png` | 返回 JSON（含 base64，可选保存） |
| GET | `/api/v1/sessions/{id}/cookies?url=...` | Cookie 列表 |
| POST | `/api/v1/sessions/{id}/cookies` | 写入 Cookie `{cookies:[...]}` |
| GET | `/api/v1/sessions/{id}/challenges` | 识别二维码 / OTP / 验证码 |
| GET | `/api/v1/sessions/{id}/qr` | 返回二维码 PNG |
| POST | `/api/v1/sessions/{id}/click_xy` | `{x, y}` |
| POST | `/api/v1/sessions/{id}/scroll` | `{dx, dy}` |

### Chrome 版本

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/chrome` | 当前 Chrome 版本与目录 |
| GET | `/api/v1/chrome/versions` | 已安装的托管版本 |
| POST | `/api/v1/chrome/update` | 下载并安装最新稳定版 |
| POST | `/api/v1/chrome/install` | 上传 `.deb`（multipart `file`）或 `{url}` |
| PUT | `/api/v1/chrome/use` | 设置全局默认版本 `{version}` |
| DELETE | `/api/v1/chrome/versions/{version}` | 删除托管版本 |
| PUT | `/api/v1/envs/{id}/chrome` | 为环境绑定版本 `{version}` |
| DELETE | `/api/v1/envs/{id}/chrome` | 解除环境版本绑定 |
| PUT | `/api/v1/user/chrome` | 设置用户默认版本 `{version}` |
| DELETE | `/api/v1/user/chrome` | 取消用户默认版本 |

版本目录默认 `/opt/autochrome/chrome`，可用 `AUTOCHROME_CHROME_DIR` 覆盖。
`version` 可以是托管版本号、`system` 或 `default`。

### 附件

附件默认存放在用户 `attachment_root`（默认 `data/users/<id>/attachments` 或
`data/attachments`），索引在 `data/users/<id>/attachments.json`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/attachments` | 附件列表 |
| POST | `/api/v1/attachments` | multipart 上传，字段 `file`，可选 `name` |
| GET | `/api/v1/attachments/{id}` | 附件元数据 |
| PATCH | `/api/v1/attachments/{id}` | 重命名 `{name}` |
| GET | `/api/v1/attachments/{id}/download` | 下载原始文件 |
| DELETE | `/api/v1/attachments/{id}` | 删除 |

单文件上限 512MB。

### 容器附件

每个容器有独立附件目录 `<容器目录>/attachments`，与通用 `/attachments` 分开：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/containers/{name}/attachments` | 附件列表 |
| POST | `/api/v1/containers/{name}/attachments` | multipart 上传 `file` |
| GET | `/api/v1/containers/{name}/attachments/{aid}` | 元数据 |
| PATCH | `/api/v1/containers/{name}/attachments/{aid}` | 重命名 |
| GET | `/api/v1/containers/{name}/attachments/{aid}/download` | 下载 |
| DELETE | `/api/v1/containers/{name}/attachments/{aid}` | 删除 |

`/envs/{id}/attachments/...` 等价。

### 容器清理

```http
POST /api/v1/containers/{name}/clean
```

```json
{
  "cache": true,
  "downloads": true,
  "screenshots": false,
  "attachments": false,
  "restart": true,
  "dry_run": false
}
```

- `cache`：所有不影响登录的缓存/历史/Favicons/临时文件
- `downloads`：清空 `<容器目录>/downloads`
- `screenshots`：清空 `<容器目录>/screenshots`
- `attachments`：清空 `<容器目录>/attachments`
- `restart`：如果清理前有会话，关闭→清理→用同一 profile/代理/Chrome 版本/URL 重启；没有会话时默认只清理不打开
- `dry_run`：只返回将删除的路径和大小

始终保留 Cookie、Local Storage、Session Storage、IndexedDB、WebStorage、
Login Data、ClientCertificates、Preferences、Extensions、Service Worker、
Network/Cookies、`Local State` 和 `env.json`。

### Cookie 与 UA

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/sessions/{id}/auth` | 导出 Cookie + `user_agent` + 语言/平台 |
| POST | `/api/v1/sessions/{id}/auth` | 导入 Cookie/UA（JSON） |
| GET | `/api/v1/sessions/{id}/auth/export?download=1` | 以文件形式下载 |
| POST | `/api/v1/sessions/{id}/auth/import` | multipart `file` 导入 JSON |

导入 UA 时会通过 CDP `Emulation.setUserAgentOverride` 设置，并根据 UA 中的
Chrome 主版本重建 UA-CH；该接口只影响当前会话，不写入容器指纹。要持久化语言/
时区/UA，请使用 `/containers/{name}/fingerprint`。

导出格式：

```json
{
  "user_agent": "Mozilla/5.0 ...",
  "accept_language": "en-US",
  "platform": "Win32",
  "url": "https://example.com/",
  "title": "Example Domain",
  "cookies": [
    {"name": "sessionid", "value": "...", "domain": "example.com", "path": "/", "secure": true, "httpOnly": true}
  ],
  "exported_at": "2026-11-22T14:00:00Z"
}
```

### 交互页面

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/sessions/{id}/interaction` | 生成短期交互页面 URL（需 Bearer） |
| GET | `/i/{token}` | HTML 交互页面（截图 + 二维码 + 验证码输入 + VNC） |
| GET | `/i/{token}/state` | 当前 URL、标题、challenge 列表 |
| GET | `/i/{token}/screenshot` | 当前页面 PNG |
| GET | `/i/{token}/qr` | 二维码 PNG（无二维码返回 404） |
| GET | `/i/{token}/vnc` | 302 跳转到 noVNC |
| POST | `/i/{token}/code` | `{code, selector?, submit?}` 输入验证码 |
| POST | `/i/{token}/click_xy` | `{x, y}` 坐标点击 |

`token` 有效期 15 分钟，只绑定一个会话。拿到 URL 的人可以操作该会话，
因此不要把 URL 发到不可信的地方。页面内的图片和表单都使用相对路径。

### 脚本

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/scripts` | 脚本列表 |
| GET | `/api/v1/scripts/{name}` | 脚本源码与 manifest |
| PUT | `/api/v1/scripts/{name}` | 新增/替换 `{source, description?, timeout?, inputSchema?}` |
| DELETE | `/api/v1/scripts/{name}` | 删除脚本 |
| POST | `/api/v1/scripts/{name}/run` | `{input?, session_id?}`，需 `allow_scripts` |

### 异步脚本任务

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/scripts/{name}/jobs` | 启动异步任务 `{input?, session_id?}` |
| GET | `/api/v1/jobs` | 任务列表 |
| GET | `/api/v1/jobs/{id}` | 状态、结果、错误 |
| GET | `/api/v1/jobs/{id}/logs` | 纯文本日志 |
| DELETE | `/api/v1/jobs/{id}` | 取消任务 |

- 每用户默认并发 10，可用 `AUTOCHROME_JOB_CONCURRENCY` 覆盖
- 元数据在 `data/users/<id>/jobs.json`
- 日志在 `data/logs/jobs/<job_id>.log`
- 默认保留 7 天；进程重启时未完成任务标记为 `interrupted`
- MCP 客户端使用 `script_job_start/status/logs/cancel/list`

### VNC

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/desktop` | 本地 noVNC 地址与端口（启用 `--vnc` 时） |
| POST | `/api/v1/desktop/token` | 生成公网 noVNC token，返回 `/vnc/{token}/...` |
| GET | `/vnc/{token}/...` | 代理 noVNC 页面、静态资源和 WebSocket |

token 默认 **300 秒无动作自动失效并断开**，可用
`AUTOCHROME_VNC_IDLE_SECONDS` 调整。VNC 端口仍只监听 `127.0.0.1`，
公网访问统一走 API 端口，建议放在 TLS 反向代理后面。

## 5. 示例

```bash
TOKEN=$(cat data/users/default/api_token)
BASE=http://127.0.0.1:8787/api/v1
H="Authorization: Bearer $TOKEN"

# 创建环境
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"id":"account-01"}' "$BASE/envs"

# 打开浏览器
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"env":"account-01","url":"https://example.com"}' "$BASE/sessions"

# 读取快照
SID=...   # 上一步返回的 session_id
curl -s -H "$H" "$BASE/sessions/$SID/snapshot"

# 点击 ref
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"ref":"e1"}' "$BASE/sessions/$SID/click"

# 截图
curl -s -H "$H" "$BASE/sessions/$SID/screenshot" -o shot.png

# 等待登录完成（扫码/验证码之后）
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"url_contains":"/home","timeout_ms":120000}' \
  "$BASE/sessions/$SID/wait"

# 新增代理
PROXY=$(curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"name":"fr-1","url":"socks5://127.0.0.1:10801"}' "$BASE/proxies")
PID=$(echo "$PROXY" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')

# 检测代理
curl -s -X POST -H "$H" "$BASE/proxies/$PID/check"

# 绑定到环境
curl -s -X PUT -H "$H" -H 'Content-Type: application/json' \
  -d "{\"proxy_id\":\"$PID\"}" "$BASE/envs/account-01/proxy"

# 用环境绑定的代理打开会话
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"env":"account-01","url":"https://example.com"}' "$BASE/sessions"

# 运行脚本
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"input":{"url":"https://example.com"}}' \
  "$BASE/scripts/example/run"

# 上传附件
curl -s -X POST -H "$H" -F 'file=@./avatar.png' "$BASE/attachments"

# 导出/导入环境
curl -s -H "$H" "$BASE/envs/account-01/export" -o account-01.tar.gz
curl -s -X POST -H "$H" -F 'file=@account-01.tar.gz' "$BASE/envs/import"

# 导出/导入 Cookie + UA
curl -s -H "$H" "$BASE/sessions/$SID/auth/export?download=1" -o auth.json
curl -s -X POST -H "$H" -F 'file=@auth.json' "$BASE/sessions/$SID/auth/import"

# 生成交互页面（扫码/验证码/打码人工接管）
curl -s -H "$H" "$BASE/sessions/$SID/interaction"

# VNC 地址
curl -s -H "$H" "$BASE/desktop"
```

## 6. 截图返回形式

默认返回原始 PNG：

```bash
curl -s -H "$H" "$BASE/sessions/$SID/screenshot" -o shot.png
```

需要 base64 或保存到用户截图目录：

```bash
curl -s -H "$H" "$BASE/sessions/$SID/screenshot?json=1&name=shot.png"
```

`name` 只取文件名，不能跳出用户截图目录。

## 7. 权限

- `/eval` 需要用户 `allow_eval=true`，或启动时 `--allow-eval`
- `/scripts/{name}/run` 需要 `allow_scripts=true`，或启动时 `--allow-scripts`
- 新用户默认关闭这两个权限：

```bash
./bin/autochrome user add alice --allow-eval --allow-scripts
```

## 8. 多用户

一个 `serve` 进程只服务一个用户。多用户时启动多个端口：

```bash
./bin/autochrome serve -user alice -addr 127.0.0.1:8787
./bin/autochrome serve -user bob   -addr 127.0.0.1:8788
```

每个用户有独立的 `api_token`、会话、脚本、截图和浏览器槽位。

## 9. 长请求

`wait`、脚本、截图等可能耗时较长。HTTP server 不设置写超时，但建议客户端
自己设置合理超时，并在业务层使用 `timeout_ms`。

## 10. 事件流与调度

### 事件流

`GET /api/v1/events` 返回 `text/event-stream`，事件包括
`session.opened`、`session.closed`、`env.created`、`env.updated`、
`env.deleted`、`proxy.*`、`storage.updated`、`attachment.*`、
`job.created`、`job.updated`、`schedule.*`、`token.rotated` 等。
浏览器端建议用 `fetch` 流式读取并携带 Bearer token。

### 调度

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/schedules` | 任务列表 |
| POST | `/api/v1/schedules` | 新增任务 |
| GET | `/api/v1/schedules/{id}` | 任务详情 |
| PATCH | `/api/v1/schedules/{id}` | 修改任务 |
| DELETE | `/api/v1/schedules/{id}` | 删除任务 |
| POST | `/api/v1/schedules/{id}/run` | 立即执行一次 |

任务类型：`script`（运行脚本）、`open`（定时打开环境）、`close`（关闭环境）。
`interval_seconds` 最小 5 秒。

## 11. 其他限制

- 隔离级别仍是 L2（每用户一个进程），不是 OS 用户/容器级隔离
- 不内置第三方打码服务
- 旧版 Chrome 需要自行提供 `.deb` 后通过 `/chrome/install` 安装
- 远程 VNC 需要 API 端口可达，并建议配置 TLS 反向代理

## 12. 安全

- 默认只监听 `127.0.0.1`
- 必须携带 token
- 需要远程访问时放在 TLS 反向代理后面，或使用 SSH 隧道
- 不要把端口直接暴露到公网
- `--cors` 只在明确需要浏览器跨域调用时设置
- 交互页面 token 等同于该会话的临时控制权，按敏感信息处理
