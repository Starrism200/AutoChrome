# MCP 服务器

MCP（Model Context Protocol）是 AI 客户端调用外部工具的标准协议。
AutoChrome 以 stdio MCP 服务器形式运行，AI 客户端（Claude Desktop、Claude Code、
Cursor、VS Code、opencode 等）可以直接打开浏览器、读取页面、点击和输入。

## 1. 启动

```bash
./bin/autochrome mcp                      # 默认用户
./bin/autochrome mcp -user alice          # 只暴露 alice 的目录和脚本
```

每个用户启动一个独立进程，这是 L2 隔离：独立目录、独立脚本、独立并发槽位、
独立浏览器会话。

## 2. 客户端配置

opencode / Claude Desktop / Cursor 等通用写法：

```json
{
  "mcpServers": {
    "autochrome": {
      "command": "/path/to/AutoChrome/bin/autochrome",
      "args": ["mcp", "-user", "alice"]
    }
  }
}
```

如果客户端的工作目录不是项目根目录，设置：

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

## 3. 工具

| 工具 | 作用 |
|---|---|
| `env_create` | 创建环境目录和确定性指纹 |
| `env_list` | 列出当前用户的环境 |
| `browser_open` | 启动一个会话，返回 `session_id` |
| `browser_close` | 关闭会话 |
| `browser_list` | 列出存活会话 |
| `browser_navigate` | 导航到 URL |
| `browser_snapshot` | 返回带 `e1`、`e2` 引用的页面文本快照 |
| `browser_click` | 按 ref / CSS / 可见文字点击 |
| `browser_type` | 聚焦并输入，可自动回车 |
| `browser_press` | 发送按键 |
| `browser_wait` | 等待选择器或文字 |
| `browser_screenshot` | 截图，可内联返回 PNG |
| `browser_cookies` | 读取 Cookie |
| `browser_detect_challenge` | 识别二维码 / OTP / 验证码元素 |
| `browser_qr` | 截取二维码并作为图片返回 |
| `browser_click_xy` | 按视口坐标点击（图片验证码） |
| `browser_scroll` | 滚动页面 |
| `desktop_info` | 返回 noVNC 地址（启用 `--vnc` 时） |
| `browser_eval` | 页面内执行 JS（需 `allow_eval`） |
| `script_list` | 列出当前用户的 JS 脚本 |
| `script_run` | 运行脚本（需 `allow_scripts`） |

`browser_wait` 支持同时等待 `selector`、`hidden_selector`、`text`、
`url_contains`、`cookie_name`，用于判断扫码/验证码是否完成。

### Resources

| URI | 内容 |
|---|---|
| `autochrome://envs` | 当前用户的环境列表（JSON） |
| `autochrome://sessions` | 当前存活的会话（JSON） |
| `autochrome://desktop` | noVNC 地址与端口（启用 VNC 时） |

## 4. 推荐调用顺序

```text
env_create {id: "account-01"}
browser_open {env: "account-01", url: "https://example.com"}
browser_snapshot {session_id: "..."}
browser_click {session_id: "...", ref: "e3"}
browser_type {session_id: "...", ref: "e5", text: "hello", submit: true}
browser_screenshot {session_id: "...", inline: true}
browser_close {session_id: "..."}
```

`browser_open` 返回的 `session_id` 在后续调用中复用。会话会一直保持，直到
`browser_close` 或 MCP 进程退出。

## 5. 脚本工具

`script_list` / `script_run` 与 CLI 的 `autochrome script` 共用同一套
JavaScript SDK（见 [SCRIPT_SDK.md](SCRIPT_SDK.md)）。

```json
{
  "name": "example",
  "input": {"url": "https://example.com"},
  "session_id": "optional-existing-session"
}
```

如果传入 `session_id`，脚本内可通过 `input.session_id` 使用
`ac.session(input.session_id)` 复用该会话。

## 6. 人工接管

启动时加 `--vnc` 即可同时提供 noVNC：

```bash
./bin/autochrome mcp -user alice --vnc
```

AI 遇到二维码 / 短信验证码 / 图片验证码时，可以：

1. `browser_detect_challenge` 识别类型；
2. `browser_qr` 或 `browser_screenshot` 返回图片；
3. `desktop_info` 返回 noVNC 地址，让用户直接接管；
4. `browser_wait` 等待登录/验证完成。

详见 [INTERACTION.md](INTERACTION.md) 和 [VNC.md](VNC.md)。

## 7. 权限

- `browser_eval` 仅在用户 `allow_eval=true` 或启动参数 `--allow-eval` 时注册
- `script_list` / `script_run` 仅在 `allow_scripts=true` 或 `--allow-scripts` 时注册
- 新用户默认关闭这两个权限：

```bash
./bin/autochrome user add alice --allow-eval --allow-scripts
```

默认用户默认开启，便于本地使用。

## 8. 运行机制

- 传输：stdio，stdout 只输出 JSON-RPC，日志写 stderr
- 虚拟显示：服务器启动一个共享 Xvfb，所有会话复用
- 会话：每个会话一个 Chrome 进程和一个持久化 profile
- 并发：受用户 `max_browsers` 和 `browserslot` 限制
- 退出：客户端断开后自动关闭所有会话和 Xvfb
