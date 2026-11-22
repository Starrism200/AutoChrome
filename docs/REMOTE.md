# 远程部署：REST + MCP

Server A 跑一个 `serve` 进程，同时提供 REST API、MCP Streamable HTTP、noVNC
和交互页面。Server B/C 作为 MCP 客户端或 REST 客户端远程调用。

```text
Server A: autochrome serve -user alice --mcp --addr 0.0.0.0:8787
  ├─ /api/v1/*        REST API
  ├─ /mcp             MCP Streamable HTTP
  ├─ /vnc/<token>/*   noVNC
  └─ /i/<token>       交互页面

共享：users.User、session.Manager、jobs.Manager、脚本目录、API token
```

一个用户一个进程，符合现有 L2 隔离。REST 和 MCP 共享同一个浏览器会话管理器。

## 1. 启动

```bash
./bin/autochrome serve -user alice --mcp --addr 0.0.0.0:8787
```

token：

```text
data/users/alice/api_token
```

首次启动会生成；也可以使用：

```bash
./bin/autochrome serve -user alice --mcp --addr 0.0.0.0:8787 --token mytoken
```

当前按需求使用 **HTTP + Bearer token**，没有启用 TLS。公网部署时建议至少加
防火墙规则；后续需要 TLS 时再加证书参数。

## 2. 远程 MCP 客户端

客户端配置示例：

```json
{
  "mcpServers": {
    "autochrome": {
      "type": "streamable-http",
      "url": "http://serverA:8787/mcp",
      "headers": {
        "Authorization": "Bearer <api_token>"
      }
    }
  }
}
```

不同客户端字段名可能是 `type` / `transport` / `headers`，以客户端文档为准。

Go 客户端：

```go
client := mcp.NewClient(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
transport := &mcp.StreamableClientTransport{
    Endpoint:   "http://serverA:8787/mcp",
    HTTPClient: &http.Client{Transport: bearerTransport{token: "..."}},
}
sess, err := client.Connect(ctx, transport, nil)
```

MCP 工具包括浏览器操作、`script_list`、`script_run` 和异步 job 工具：

- `script_job_start`
- `script_job_status`
- `script_job_logs`
- `script_job_cancel`
- `script_job_list`

脚本上传统一走 REST，不通过 MCP 上传。

## 3. 远程执行脚本

### 同步（短脚本）

```bash
TOKEN=...
BASE=http://serverA:8787/api/v1

curl -X PUT -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"source":"return {ok:true};","timeout":"60s"}' \
  "$BASE/scripts/demo"

curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"input":{"foo":"bar"}}' \
  "$BASE/scripts/demo/run"
```

### 异步（长脚本）

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"input":{"foo":"bar"}}' \
  "$BASE/scripts/demo/jobs"
```

返回 `job_id`，然后：

```bash
curl -H "Authorization: Bearer $TOKEN" "$BASE/jobs/$JOB_ID"
curl -H "Authorization: Bearer $TOKEN" "$BASE/jobs/$JOB_ID/logs"
curl -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/jobs/$JOB_ID"
```

- 每个用户默认并发 **10**
- 可用 `AUTOCHROME_JOB_CONCURRENCY` 覆盖
- 元数据：`data/users/<id>/jobs.json`
- 日志：`data/logs/jobs/<job_id>.log`
- 默认保留 7 天
- 进程重启时，未完成的 job 标记为 `interrupted`

MCP 客户端等价流程：

```text
script_job_start → script_job_status / script_job_logs → 完成
```

## 4. 远程 MCP 调用示例

短任务：

```text
browser_open {env:"c1", url:"https://example.com"}
browser_snapshot {session_id:"..."}
script_run {name:"demo", input:{...}}
```

长任务：

```text
script_job_start {name:"demo", input:{...}}
script_job_status {job_id:"..."}
script_job_logs {job_id:"..."}
```

## 5. 部署注意

- 每用户一个端口/进程，例如 alice:8787、bob:8788
- `/mcp` 和 `/api/v1` 使用同一个 API token
- `allow_scripts` 控制脚本上传和执行
- 直接暴露时至少限制来源 IP/防火墙
- VNC 仍只监听 `127.0.0.1`，公网走 `/vnc/<token>/...`
- 长任务用 jobs，不要依赖同步 `/run` 的 HTTP 超时
