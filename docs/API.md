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
--allow-eval           # 强制开启 /eval
--allow-scripts        # 强制开启脚本运行
```

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
| GET | `/api/v1/user` | 当前用户与配置 |

### 环境

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/envs` | 环境列表 |
| POST | `/api/v1/envs` | 创建环境 `{id, platform?}` |

### 会话

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/sessions` | 存活会话列表 |
| POST | `/api/v1/sessions` | 打开会话 `{env, platform?, url?, proxy?, headless?}` |
| GET | `/api/v1/sessions/{id}` | 会话信息 |
| DELETE | `/api/v1/sessions/{id}` | 关闭会话 |

### 页面

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/sessions/{id}/navigate` | `{url}` |
| GET | `/api/v1/sessions/{id}/snapshot?max=300` | 带 `e1` 引用的文本快照 |
| POST | `/api/v1/sessions/{id}/click` | `{ref?, selector?, text?}` |
| POST | `/api/v1/sessions/{id}/type` | `{ref?, selector?, text, submit?}` |
| POST | `/api/v1/sessions/{id}/press` | `{key}` |
| POST | `/api/v1/sessions/{id}/wait` | `{selector?, hidden_selector?, text?, url_contains?, cookie_name?, timeout_ms?}` |
| POST | `/api/v1/sessions/{id}/eval` | `{expression}`，需 `allow_eval` |
| GET | `/api/v1/sessions/{id}/screenshot` | 返回 `image/png` |
| GET | `/api/v1/sessions/{id}/screenshot?json=1&name=a.png` | 返回 JSON（含 base64，可选保存） |
| GET | `/api/v1/sessions/{id}/cookies?url=...` | Cookie 列表 |
| GET | `/api/v1/sessions/{id}/challenges` | 识别二维码 / OTP / 验证码 |
| GET | `/api/v1/sessions/{id}/qr` | 返回二维码 PNG |
| POST | `/api/v1/sessions/{id}/click_xy` | `{x, y}` |
| POST | `/api/v1/sessions/{id}/scroll` | `{dx, dy}` |

### 脚本

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/scripts` | 脚本列表 |
| POST | `/api/v1/scripts/{name}/run` | `{input?, session_id?}`，需 `allow_scripts` |

### VNC

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/desktop` | noVNC 地址与端口（启用 `--vnc` 时） |

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

# 运行脚本
curl -s -X POST -H "$H" -H 'Content-Type: application/json' \
  -d '{"input":{"url":"https://example.com"}}' \
  "$BASE/scripts/example/run"

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

## 10. 安全

- 默认只监听 `127.0.0.1`
- 必须携带 token
- 需要远程访问时放在 TLS 反向代理后面，或使用 SSH 隧道
- 不要把端口直接暴露到公网
- `--cors` 只在明确需要浏览器跨域调用时设置
