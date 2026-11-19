---
name: lightpanda
description: 优先使用 Lightpanda 进行网页导航、DOM、表单和 JavaScript；截图及实际渲染使用备用 CloakBrowser。 Prefer Lightpanda for navigation, DOM, forms and JavaScript; use CloakBrowser for screenshots and actual rendering.
github_url: https://github.com/lightpanda-io/browser
---

Discover `lightpanda` with `tool_search` and select the returned stable ID. It is the first browser choice; `cloakbrowser` is the backup for unavailable Lightpanda, screenshots, actual layout/rendering, native zoom or unsupported APIs. A headless DOM observation does not prove visual appearance.

Use synchronous Playwright Python through the local Lightpanda CDP server. Variables are `page`, `context`, `browser`, `args` and `result`. Keep literal input in args; assign JSON observations to result. Related navigation, filling, submission and verification belong in one call, because each call has its own browser/context and closes it afterward. No persistent login or personal browser session is attached.

~~~json
{"code":"page.goto(args['url']); result={'title':page.title(),'text':page.locator('body').inner_text()}","args":{"url":"https://example.com"}}
~~~

Set `LIGHTPANDA_BINARY_PATH` to a preinstalled frozen binary, or install lightpanda on PATH. Python playwright and psutil are protocol/lifecycle dependencies; no standalone browser CLI or downloaded Chromium is used. Discovery checks prerequisites and never launches a browser or downloads files. Lightpanda is headless and needs no Xvfb. Results identify the backend and observed cleanup status.

Errors propagate. An action may have changed the remote page before failing. Inspect saved effects before selecting the backup; do not automatically retry code or replay an uncertain submission across browsers. Cleanup completion is local process cleanup, not rollback of a web operation. Installation and validation boundaries: examples/lightpanda.md.
