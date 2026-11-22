# JavaScript 脚本 SDK

AutoChrome 内置一个同步 JavaScript 运行时（基于 goja），不需要 Node.js。
脚本通过全局 `ac` 对象操作浏览器，适合由 AI 生成。

## 1. 脚本位置

| 用户 | 目录 |
|---|---|
| `default` | `scripts/custom/` |
| 其它用户 | `data/users/<id>/scripts/` |

每个脚本由两个文件组成：

```text
scripts/custom/example.js     # 脚本源码
scripts/custom/example.json   # 可选 manifest
```

manifest：

```json
{
  "description": "Open a page and return its title",
  "timeout": "60s",
  "inputSchema": {
    "type": "object",
    "properties": {
      "url": { "type": "string" }
    }
  }
}
```

`timeout` 支持 `"30s"`、`"2m"` 或秒数。未提供时默认 60 秒。

## 2. 运行

```bash
./bin/autochrome script list
./bin/autochrome script run example -input '{"url":"https://example.com"}'
./bin/autochrome script run example -input-file input.json
./bin/autochrome script run example -user alice -input '{"url":"https://example.com"}'
```

脚本返回值会以 JSON 输出到 stdout。

## 3. 全局变量

| 名称 | 说明 |
|---|---|
| `input` | 调用方传入的 JSON 对象 |
| `ac` | AutoChrome SDK |
| `console.log/error/warn` | 写入 stderr |

脚本必须使用同步写法，不支持顶层 `await`。最后用 `return` 返回 JSON 可序列化的值。

## 4. `ac` API

| 方法 | 说明 |
|---|---|
| `ac.open({env, platform?, url?, proxy?, headless?})` | 打开环境并返回 `page` |
| `ac.session(sessionId)` | 复用已有 MCP/CLI 会话 |
| `ac.env.create({id, platform?})` | 创建环境目录，返回 `{dir, fingerprint}` |
| `ac.env.list()` | 列出当前用户的环境 |
| `ac.log(...)` | 写日志 |
| `ac.user` | 当前用户 id |

## 5. `page` API

| 方法 | 说明 |
|---|---|
| `page.goto(url)` | 导航 |
| `page.reload()` | 刷新当前页面 |
| `page.back()` | 后退 |
| `page.url()` / `page.title()` | 当前 URL / 标题 |
| `page.snapshot({max?})` | 返回 `{url, title, nodes}`，每个节点带 `ref` |
| `page.click(target)` | 点击 |
| `page.type(target, text)` | 聚焦并输入 |
| `page.press(key)` | 按键，如 `Enter`、`Tab`、`Escape` |
| `page.wait({selector?, hiddenSelector?, text?, urlContains?, cookie?, timeout?})` | 等待条件全部满足，`timeout` 毫秒 |
| `page.eval(expression)` | 页面内执行 JS，自动 await Promise |
| `page.screenshot({path?, fullPage?, inline?})` | 截图；相对路径在用户截图目录下 |
| `page.cookies({urls?})` | 读取 Cookie |
| `page.detectChallenge()` | 识别二维码 / OTP / 验证码 |
| `page.qr()` | 截取二维码，返回 `{selector, name, base64}` |
| `page.clickXY(x, y)` | 按视口坐标点击 |
| `page.scroll(dx, dy)` | 滚动页面 |
| `page.close()` | 关闭本次打开的会话 |

### target 形式

```js
page.click("e3");                       // snapshot ref
page.click("#submit");                  // CSS selector
page.click({text: "Sign in"});          // 可见文字（真实 Enter 事件）
page.type("e5", "hello@example.com");   // ref
page.type({selector: "input[name=q]"}, "AutoChrome");
```

## 6. 示例

```js
const page = ac.open({
  env: input.env || "example",
  url: input.url || "https://example.com",
});

const snap = page.snapshot({ max: 100 });
console.log("title:", page.title());
console.log("nodes:", snap.nodes.length);

const summary = {
  title: page.title(),
  url: page.url(),
  nodes: snap.nodes.slice(0, 5),
};

page.screenshot({ path: "example.png", fullPage: true });
page.close();
return summary;
```

## 7. 给 AI 的生成提示词

```text
You write AutoChrome scripts in synchronous JavaScript.

Available globals: input (object), ac, console.
Do not use async/await or imports.
Use ac.open({env, url, proxy?, headless?}) to get a page.
Call page.snapshot() first, then use refs like "e3" with page.click/type.
Return one JSON-serializable value with `return`.
Keep the script under 80 lines and avoid hard-coded credentials.

API:
ac.open, ac.session, ac.env.create, ac.env.list, ac.log, ac.user
page.goto, page.url, page.title, page.snapshot, page.click, page.type,
page.press, page.wait, page.eval, page.screenshot, page.cookies, page.close
```

把用户的自然语言需求连同这段提示词一起交给模型，再执行：

```bash
./bin/autochrome script run <name> -input '<json>'
```

## 8. 交互处理示例

```js
const page = ac.open({ env: input.env, url: input.url });

const challenges = page.detectChallenge();
if (challenges.some((c) => c.type === "qr")) {
  const qr = page.qr();
  console.log("QR:", qr.selector, qr.base64.length);
  // 真人在 noVNC 扫码，或把 base64 交给客户端展示
}

page.wait({
  hiddenSelector: "[data-ac-challenge^='ac-qr']",
  urlContains: "/home",
  timeout: 180000,
});

return { url: page.url(), title: page.title() };
```

更多说明见 [INTERACTION.md](INTERACTION.md)。

## 9. 权限

- 用户的 `user.json` 中 `allow_eval` 控制 `page.eval`
- `allow_scripts` 控制是否允许运行脚本
- 通过 `autochrome user add <id> --allow-eval --allow-scripts` 开启
