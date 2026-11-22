# AutoChrome

浏览器 + 虚拟显示器（Xvfb）工具集：为每个环境生成独立的 Chrome profile 和
确定性指纹，并提供内置 JavaScript 脚本 SDK 与 MCP 服务器，方便 AI 驱动 Chrome
或生成自动化脚本。

[English](README.md) | 简体中文

## 功能

- 一键安装 Google Chrome、Xvfb、字体等系统依赖（`install.sh`）
- 一键升级 Chrome / 系统依赖并重新编译（`update.sh`）
- 容器/环境有唯一 name/id 和独立目录
- 支持容器新增、修改、查询、删除、导入、导出
- 基于种子确定性生成浏览器指纹（UA、UA-CH、时区、语言、屏幕、WebGL、CPU/内存）
- 在没有物理显示器时自动用 `xvfb-run` 启动有头 Chrome
- 支持按环境绑定代理
- 跨进程浏览器并发上限（`flock` 槽位）
- 内置同步 JavaScript SDK（`ac` / `page`），无需 Node.js
- stdio MCP 服务器，提供浏览器工具与资源
- HTTP/JSON API（`autochrome serve`），Bearer Token 认证
- 代理管理、按实例绑定代理、代理连通性检测
- 可配置环境/附件存储路径，通用附件上传下载
- 环境打包导出/导入（`.tar.gz`）
- Chrome 多版本管理（安装/切换/更新/删除）
- API 代理 noVNC 公网 token，300 秒无动作断开
- SSE 事件流、批量打开会话、token 轮换、定时任务
- 远程 MCP（Streamable HTTP，`serve --mcp`）
- 异步脚本任务，带持久化日志和取消
- OpenAPI 文档：`/api/v1/openapi.yaml`
- Cookie + User-Agent 导入导出
- 短期交互页面，用于扫码 / 验证码 / 打码人工接管
- VNC/noVNC 远程桌面，支持真人接管（扫码、短信验证码、打码）
- 二维码/OTP/验证码检测、二维码截图与坐标点击
- 多用户隔离（L2：每用户一个 MCP 进程和独立目录树）
- CDP 能力：带元素引用的快照、截图、Cookie、真实键盘事件

## 架构

Chrome 二进制是共享的（系统 Chrome 或托管版本）；每个容器（环境）有唯一的
name/id 和独立目录，每个存活会话是一个独立的 Chrome 进程。用户层面是 L2 隔离：
独立进程 + 独立目录，不是 OS 用户或容器隔离。

## 依赖

- Linux
- Go 1.26+（可用 `GO_BIN` 指定工具链）
- Google Chrome
- `xvfb`
- `fonts-liberation`
- `x11vnc`、`novnc`、`websockify`（VNC 需要，`install.sh` 会安装）
- `dpkg-deb`（安装托管 Chrome `.deb` 包）

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

## 快速开始

```bash
sudo ./install.sh

./bin/autochrome gen -id account-01
./bin/autochrome open -id account-01 -url https://example.com
```

`gen` 会创建 `profiles/default-account-01/` 并写入 `env.json` 指纹。
`open` 在 Xvfb 中启动有头 Chrome，可同时截图：

```bash
./bin/autochrome open -id account-01 -url https://example.com \
  -screenshot shot.png -keep
```

## 多用户

`default` 用户保留历史布局；其它用户位于 `data/users/<id>/`，拥有独立的
profiles、脚本、截图、日志和浏览器槽位。

```bash
./bin/autochrome user add alice --allow-eval --allow-scripts --max-browsers 2
./bin/autochrome user list
./bin/autochrome user show alice

./bin/autochrome gen -user alice -id account-01
```

优先级：`-user` > `AUTOCHROME_USER` > `default`。
详见 [docs/MULTIUSER.md](docs/MULTIUSER.md)。

## JavaScript 脚本

脚本是同步 JavaScript，在嵌入式运行时中执行，接收 `input`、`ac`、`console`，
用 `return` 返回 JSON 可序列化的值。

```bash
./bin/autochrome script list
./bin/autochrome script run example -input '{"url":"https://example.com"}'
```

示例 `scripts/custom/example.js`：

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

完整 API 和给 AI 的提示词模板见 [docs/SCRIPT_SDK.md](docs/SCRIPT_SDK.md)。

## MCP 服务器

```bash
./bin/autochrome mcp -user alice
```

客户端配置：

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

服务器提供环境、会话、导航、快照、点击、输入、等待、截图、Cookie、eval 和
脚本工具，以及 `autochrome://envs`、`autochrome://sessions` 资源。
详见 [docs/MCP.md](docs/MCP.md)。

## HTTP API

```bash
./bin/autochrome serve -user alice -addr 127.0.0.1:8787
```

服务使用用户级 token，存放在 `data/users/alice/api_token`，在 `/api/v1` 下提供
容器/环境、会话、导航、快照、截图、验证码检测、脚本、代理和 VNC 信息。还支持代理
增删改查/检测、按环境绑定代理，以及 `GET /api/v1/sessions/{id}/interaction`
生成的短期交互页面；以及存储路径、附件上传下载、环境打包导入导出、
Cookie+UA 导入导出、Chrome 多版本管理、SSE 事件流、批量打开、token 轮换、
间隔定时任务和短期交互页面。OpenAPI：`GET /api/v1/openapi.yaml`。

`serve --mcp` 在同一端口挂载 MCP Streamable HTTP：

```bash
./bin/autochrome serve -user alice --mcp --addr 0.0.0.0:8787
# MCP endpoint: http://serverA:8787/mcp
```

长脚本走异步任务：

```bash
POST /api/v1/scripts/{name}/jobs
GET  /api/v1/jobs/{id}
GET  /api/v1/jobs/{id}/logs
DELETE /api/v1/jobs/{id}
```

详见 [docs/REMOTE.md](docs/REMOTE.md)。

```bash
TOKEN=$(cat data/users/alice/api_token)
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8787/api/v1/sessions
```

详见 [docs/API.md](docs/API.md)。

## VNC / 人工接管

```bash
./bin/autochrome mcp -user alice --vnc
```

公网访问调用 `POST /api/v1/desktop/token` 获取短期 `/vnc/<token>/...` 地址，
由 API 代理 noVNC；默认 300 秒无动作自动断开。本机 noVNC 地址仍会打印在
stderr，并通过 `desktop_info` / `autochrome://desktop` 暴露。

端口从用户配置的范围自动分配，默认 `10000-59999`：

```bash
./bin/autochrome user add alice --vnc-port-start 20000 --vnc-port-end 20999
```

独立服务方式：

```bash
./bin/autochrome desktop -user alice
DISPLAY=:100 ./bin/autochrome mcp -user alice
```

详见 [docs/VNC.md](docs/VNC.md) 和 [docs/INTERACTION.md](docs/INTERACTION.md)。

## 目录结构

```text
cmd/autochrome/        命令行入口：gen / open / user / script / mcp
internal/browser/      Chrome 启动、CDP 操作、快照、截图
internal/browserslot/  跨进程浏览器并发槽位
internal/envdir/       环境目录、索引与打包
internal/attachments/  通用附件存储
internal/fingerprint/  确定性浏览器指纹生成
internal/users/        用户目录、配额与权限
internal/session/      长生命周期浏览器会话
internal/jsruntime/    嵌入式 JavaScript SDK（goja）
internal/scripts/      脚本发现与 manifest
internal/mcpserver/    MCP stdio 服务器
internal/apiserver/    HTTP/JSON API 服务器
internal/chrome/       Chrome 多版本管理
internal/scheduler/    间隔定时任务
internal/jobs/         异步脚本任务与日志
internal/filelock/     跨进程索引锁
internal/atomicfile/   原子文件写入
internal/xvfb/         共享虚拟显示器
internal/desktop/      VNC/noVNC 远程桌面服务
internal/ua/           UA 常量
scripts/common.sh      GO_BIN / CHROME / 构建 / xvfb_run
scripts/custom/        示例用户脚本
install.sh             系统依赖 + Chrome + 构建
update.sh              升级 + 重新构建
docs/                  安装、浏览器、SDK、MCP 与多用户文档
```

## 文档

- [docs/INSTALL.md](docs/INSTALL.md)：安装、更新、依赖排查
- [docs/BROWSER.md](docs/BROWSER.md)：虚拟显示器、指纹、启动参数、并发与代理
- [docs/SCRIPT_SDK.md](docs/SCRIPT_SDK.md)：JavaScript 脚本 API 与 AI 提示词
- [docs/MCP.md](docs/MCP.md)：MCP 工具、资源与客户端配置
- [docs/MULTIUSER.md](docs/MULTIUSER.md)：多用户模型、目录与权限
- [docs/VNC.md](docs/VNC.md)：x11vnc + noVNC、端口与安全
- [docs/INTERACTION.md](docs/INTERACTION.md)：扫码、验证码、打码的人工接力
- [docs/API.md](docs/API.md)：HTTP/JSON API、认证与示例
- [docs/REMOTE.md](docs/REMOTE.md)：远程 REST + MCP、脚本任务与部署
