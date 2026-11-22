# 交互处理：扫码、验证码、打码

AutoChrome 采用 **人工接力**：AI 负责导航和常规操作，遇到必须真人的步骤时，
把控制权交给用户。推荐配合 [VNC / noVNC](VNC.md) 使用。

## 1. 工作流

```text
AI: browser_open
AI: browser_snapshot / browser_screenshot
AI: browser_detect_challenge
      ├─ qr      → browser_qr 返回二维码图片，或直接用 noVNC 让用户扫码
      ├─ otp     → 用户在 noVNC 输入，或 AI 询问后 browser_type
      └─ captcha → 用户在 noVNC 处理，或 AI 截图后 browser_click_xy
AI: browser_wait {url_contains | cookie_name | hidden_selector | text}
AI: 继续后续操作
```

## 2. 相关工具

| 工具 | 用途 |
|---|---|
| `browser_detect_challenge` | 启发式识别二维码、OTP 输入框、验证码元素 |
| `browser_qr` | 截取第一个二维码并作为图片返回 |
| `browser_click_xy` | 按视口坐标点击（图片验证码） |
| `browser_scroll` | 滚动页面 |
| `browser_wait` | 等待 URL / Cookie / 元素出现或消失 |
| `desktop_info` | 返回 noVNC 地址，供人工接管 |
| `browser_screenshot` | 返回整页或视口截图 |

### `browser_wait` 条件

```json
{
  "session_id": "...",
  "url_contains": "/home",
  "cookie_name": "sessionid",
  "selector": ".avatar",
  "hidden_selector": ".qrcode",
  "timeout_ms": 120000
}
```

所有给出的条件必须同时满足。

## 3. 扫码

1. 打开登录页，出现二维码。
2. `browser_detect_challenge` 返回 `type: "qr"` 和一个 selector。
3. `browser_qr` 返回裁剪后的 PNG。
4. 用户用手机扫码。
5. `browser_wait` 等待登录成功（URL 变化、Cookie 出现、二维码消失）。

二维码会过期刷新，建议：

```json
{"session_id":"...","hidden_selector":"[data-ac-challenge^='ac-qr']","timeout_ms":180000}
```

如果二维码刷新导致登录未完成，重新调用 `browser_qr` 再让用户扫一次。

## 4. 短信 / 邮箱验证码

- 最简单：用户在 noVNC 里直接输入。
- AI 辅助：AI 在对话里询问验证码，拿到后：

```json
{"session_id":"...","ref":"e7","text":"123456","submit":true}
```

- 不要在工作流中记录验证码；用完即弃。

## 5. 打码 / CAPTCHA

默认 **人工处理**：

1. `browser_detect_challenge` 找到验证码元素。
2. `browser_qr` 类似地截取元素（或直接 `browser_screenshot`）。
3. 用户在 noVNC 中填写/点选。
4. `browser_click_xy` 可用于按坐标点击图片验证码。
5. `browser_wait` 等待验证通过。

AutoChrome 不内置自动破解或第三方打码服务。第三方打码涉及平台风控、
服务条款和合规风险，如需接入应作为独立插件并自行评估。

## 6. JS SDK

```js
const page = ac.open({ env: "alice", url: "https://example.com/login" });

const challenges = page.detectChallenge();
for (const c of challenges) {
  if (c.type === "qr") {
    const qr = page.qr();          // {selector, name, base64}
    console.log("scan this QR:", qr.base64.length);
  }
}

// 等待人工完成
page.wait({ hiddenSelector: "[data-ac-challenge^='ac-qr']", timeout: 180000 });
page.wait({ urlContains: "/home", timeout: 120000 });

// 坐标点击（图片验证码）
page.clickXY(320, 180);
```

## 7. 限制

- 检测是启发式的，不保证识别所有站点；AI 应同时查看快照和截图。
- 验证码/二维码/短信验证需要真人，无法保证全自动。
- 二维码会刷新，验证码有时效，必须设置合理超时。
- 长等待期间客户端可能超时，建议使用 `timeout_ms` 并分段轮询。
