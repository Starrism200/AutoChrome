# 多用户

AutoChrome 支持按用户隔离目录、脚本、会话、并发和权限。当前实现是
**L2 隔离**：每个用户一个独立的 MCP 进程，目录完全分开，但共享同一个
操作系统账号。

## 1. 目录

`default` 用户保留历史布局：

```text
profiles/
scripts/custom/
data/screenshots/
data/logs/
data/browser-slots/
```

其它用户：

```text
data/users/<id>/
├── user.json          # 配额、权限、默认代理
├── profiles/          # 浏览器环境
├── scripts/           # JS 脚本
├── screenshots/       # 截图
├── logs/              # 日志
└── browser-slots/     # 并发槽位
```

## 2. 命令

创建用户：

```bash
./bin/autochrome user add alice
./bin/autochrome user add alice --allow-eval --allow-scripts --max-browsers 2
./bin/autochrome user add alice --proxy socks5://127.0.0.1:10801
```

查看：

```bash
./bin/autochrome user list
./bin/autochrome user show alice
```

使用某个用户：

```bash
./bin/autochrome gen -user alice -id account-01
./bin/autochrome open -user alice -id account-01 -url https://example.com
./bin/autochrome script run example -user alice -input '{"url":"https://example.com"}'
./bin/autochrome mcp -user alice
```

也可以设置环境变量：

```bash
export AUTOCHROME_USER=alice
```

优先级：`-user` > `AUTOCHROME_USER` > `default`。

## 3. `user.json`

```json
{
  "id": "alice",
  "created_at": "2026-11-22T12:00:00Z",
  "max_envs": 10,
  "max_browsers": 2,
  "allow_eval": true,
  "allow_scripts": true,
  "allowed_hosts": [],
  "default_proxy": "socks5://127.0.0.1:10801"
}
```

| 字段 | 说明 |
|---|---|
| `max_envs` | 环境数量上限（提示性） |
| `max_browsers` | 并发浏览器上限，交给 `browserslot` |
| `allow_eval` | 是否开放 `browser_eval` / `page.eval` |
| `allow_scripts` | 是否允许运行脚本 |
| `allowed_hosts` | 预留的域名白名单 |
| `default_proxy` | 该用户未指定代理时使用 |

## 4. 隔离模型

| 级别 | 方式 | 说明 |
|---|---|---|
| L1 | 同进程、不同目录 | 最轻，隔离弱 |
| **L2** | **每用户一个 MCP 进程** | **当前实现** |
| L3 | 独立系统用户 / 容器 | 最强，需要额外部署 |

L2 能做到：

- 目录隔离：profiles、scripts、screenshots、logs
- 进程隔离：一个 MCP 服务器只服务一个用户
- 并发隔离：独立槽位目录和 `max_browsers`
- 权限隔离：是否允许 eval / scripts

L2 不能做到：

- OS 级文件权限隔离（同一系统账号仍可互相读取）
- 真正的资源限额（CPU/内存/磁盘配额）
- 网络出口隔离（代理由用户配置决定）

需要更强隔离时，为每个用户使用独立的系统账号或容器，并分别启动
`autochrome mcp -user <id>`。

## 5. 项目根目录

MCP 客户端启动进程时的工作目录可能不是项目根目录。AutoChrome 按以下顺序
确定根目录：

1. `AUTOCHROME_HOME`
2. 可执行文件上一级目录（`bin/autochrome` → 项目根）
3. 当前工作目录

推荐在 MCP 配置中显式设置 `AUTOCHROME_HOME`。

## 6. 迁移

已有的 `profiles/`、`scripts/custom/`、`data/` 仍属于 `default` 用户，
无需迁移。新用户从 `data/users/<id>/` 开始。
