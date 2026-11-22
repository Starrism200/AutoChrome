# VNC / noVNC 远程操作

AutoChrome 可以把每个用户的 Xvfb 显示通过 **x11vnc + noVNC** 暴露出来，
让真人在浏览器里看到并操作 Chrome，处理扫码、短信验证码、图片验证码等交互。

资源占用很低（x11vnc 几 MB，websockify 一个 Python 进程），默认可以和 MCP
一起启动；也可以单独运行 `autochrome desktop`。

## 1. 安装依赖

```bash
sudo ./install.sh
```

会安装：

- `x11vnc`
- `novnc`
- `websockify`

## 2. 与 MCP 一起启动

```bash
./bin/autochrome mcp -user alice --vnc
```

启动后 stderr 会打印：

```text
vnc: http://127.0.0.1:16001/vnc.html?autoconnect=1&resize=scale&reconnect=1 (display :100)
```

打开该地址即可看到 Chrome 窗口并接管操作。

可选参数：

```bash
--vnc-password secret          # 设置 VNC 密码（也可用 AUTOCHROME_VNC_PASSWORD）
--vnc-port-start 10000         # 覆盖用户配置的起始端口
--vnc-port-end 59999           # 覆盖用户配置的结束端口
```

### 公网访问（API token）

对外访问不要直接暴露 noVNC 端口，统一走 API：

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://your-server:8787/api/v1/desktop/token
```

返回：

```json
{
  "token": "...",
  "url": "https://your-server/vnc/<token>/vnc.html?autoconnect=1&resize=scale&reconnect=1",
  "idle_timeout_seconds": 300
}
```

- API 服务器代理 noVNC 页面、静态资源和 WebSocket
- x11vnc/noVNC 仍只监听 `127.0.0.1`
- token 默认 **300 秒无动作自动失效并断开**
- 可用 `AUTOCHROME_VNC_IDLE_SECONDS` 调整空闲时间
- API 生成的 URL 会带 `path=vnc/<token>/websockify`，不要手动删掉；noVNC 默认会连到根路径 `/websockify`，删掉后页面能打开但会提示“无法连接到服务器”
- 建议 API 放在 TLS 反向代理后面；`--addr 0.0.0.0:8787` 或反向代理到本机

## 3. 独立 desktop 服务

如果希望 VNC 与 MCP 进程分开：

```bash
./bin/autochrome desktop -user alice
```

输出示例：

```json
{
  "display": ":100",
  "rfb_port": 16000,
  "web_port": 16001,
  "url": "http://127.0.0.1:16001/vnc.html?autoconnect=1&resize=scale&reconnect=1",
  "password": false,
  "started_at": "2026-11-22T12:55:29+08:00"
}
```

然后在另一个终端使用同一个 DISPLAY：

```bash
DISPLAY=:100 ./bin/autochrome mcp -user alice
```

`autochrome desktop` 会一直运行，按 Ctrl+C 停止。状态同时写入
`data/users/<user>/desktop.json`，方便外部程序发现。

## 4. 端口自动分配

每个用户配置自己的端口范围，默认 **10000–59999**。

创建用户时指定：

```bash
./bin/autochrome user add alice --vnc-port-start 20000 --vnc-port-end 20999
```

或编辑 `data/users/<id>/user.json`：

```json
{
  "vnc_port_start": 20000,
  "vnc_port_end": 20999
}
```

启动时会在范围内自动寻找两个空闲端口：

- `rfb_port`：x11vnc
- `web_port`：websockify / noVNC

两个端口都只监听 `127.0.0.1`。范围用尽时会报错并提示调整。

## 5. 关闭最后一个标签页后

不需要重启 `serve` 或容器，调用：

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8787/api/v1/containers/<name>/restart
```

VNC 桌面会保留，重新拉起的 Chrome 窗口会出现在同一画布上。

也可以直接刷新稳定入口，会自动检查并拉起浏览器：

```text
http://<host>:8787/vnc?token=<api_token>
```

如果同时有多个容器，可以指定：

```text
http://<host>:8787/vnc?token=<api_token>&env=<container>
```

没有指定时使用最近一次会话的容器；都没有会话时使用最近创建的容器。

## 6. NumLock / 小键盘

x11vnc 启动时使用 `-skip_lockkeys`，忽略查看器发来的 NumLock 锁键，并把
KP_n 数字映射为普通数字键。这样无论本机 NumLock 开关状态如何，小键盘数字
都能正常输入。

注意：VNC 里的 NumLock 指示灯状态可能不会和本机完全同步；这是锁键同步问题，
不是输入失效。

## 7. 画布大小

当前 noVNC 画布是**每个用户进程一个 Xvfb 屏幕**，默认 `1920x1080`；容器浏览器
默认视口是 `1366x900`。这样默认浏览器窗口不会占满画布，但设置到
`1920x1080` 时仍能完整显示。

启动时可覆盖画布大小：

```bash
./bin/autochrome serve -user alice --vnc --vnc-screen 1366x900x24
```

容器级 `PUT /containers/{name}/size` 只改变该容器 Chrome 的视口/窗口大小，
不会改变 noVNC 画布本身。如果需要每个容器独立画布大小，需要每容器独立
Xvfb/VNC，这是另一套部署模式。

## 8. 安全

默认已经做了这些限制：

- x11vnc 使用 `-localhost`
- websockify 绑定 `127.0.0.1`
- 公网访问走 API 的 `/vnc/{token}/...`，token 300 秒空闲失效
- 不使用密码时只允许本机访问

远程访问推荐 API token；也可以使用 SSH 隧道：

```bash
ssh -L 16001:127.0.0.1:16001 user@host
```

然后在本机打开 `http://127.0.0.1:16001/vnc.html`。

如果需要直接暴露，至少：

- 设置 `--vnc-password`
- 放在带 TLS 和认证的反向代理后面
- 不要直接暴露 5900 或 noVNC 端口到公网

## 9. 多用户

每个用户一个独立进程、独立 Xvfb、独立 VNC 端口，互不可见。
L2 模型下通过不同 `-user` 启动即可：

```bash
./bin/autochrome mcp -user alice --vnc --vnc-port-start 16000 --vnc-port-end 16099
./bin/autochrome mcp -user bob   --vnc --vnc-port-start 16100 --vnc-port-end 16199
```

## 10. 常见问题

**`noVNC not found`**

```bash
sudo apt-get install -y novnc
```

**`x11vnc not found` / `websockify not found`**

```bash
sudo apt-get install -y x11vnc websockify
```

**noVNC 页面空白**

确认 `web_port` 和 `rfb_port` 都在监听：

```bash
ss -ltnp | grep -E '16000|16001'
```

并确认 `DISPLAY` 与实际 Xvfb 一致。

**端口范围用尽**

扩大 `vnc_port_start` / `vnc_port_end`，或先关闭不用的 desktop 实例。
