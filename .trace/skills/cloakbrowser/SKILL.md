---
name: cloakbrowser
description: 使用有界面 CloakBrowser 完成导航、DOM、表单、JavaScript、截图和显式 set_document；每次操作隔离，固定浏览器禁用 console/pageerror，原生 set_content 写入后可能超时。 Headed CloakBrowser navigation, DOM, forms, JavaScript, screenshots and explicit set_document in one isolated action; fixed binary suppresses console/pageerror and native set_content can time out after writing.
github_url: https://github.com/CloakHQ/CloakBrowser
---

Prefer the `lightpanda` tool for navigation, DOM, forms and JavaScript. Use this backup for screenshots, actual rendering, native zoom, unsupported operations or unavailable Lightpanda. Inspect prior effects before changing browsers; do not replay an uncertain submission.

Discover `cloakbrowser` with `tool_search`, then activate its returned stable ID with `tool_select`. The tool uses the official CloakBrowser Python SDK (`cloakbrowser==0.6.4`), not a Playwright CLI or browser daemon. Python statements receive `page`, `context`, `browser`, `args` and `result`. Set `result` to JSON-serializable observations. Printed output is diagnostic stderr. Keep input values in `args`; never interpolate them into code.

~~~json
{"code":"page.goto(args['url']); result={'url':page.url,'title':page.title(),'text':page.locator('body').inner_text()}","args":{"url":"https://example.com"}}
~~~

Use native `page.locator(...)`, `page.get_by_role(...)`, `.fill(...)`, `.click()`, `page.evaluate(...)` and `page.screenshot(path=...)` APIs. Pass screenshot paths in `args`, save into the Run workspace, then register with `artifact_register`. Multi-step navigation, DOM inspection, filling, submission and outcome checks belong in the same call. Inspect actual effects before retrying a failed submission; uncertain actions are not automatically replayed.

Every call creates a fresh browser and context, launches with `headless=False`, and closes context/browser before returning. Cookies, storage and login state do not survive across calls, including calls in the same Run. No existing personal profile or default debugging port is used. On Linux without `DISPLAY`, the adapter uses an isolated `xvfb-run` virtual display; Chromium remains headed, but this is not a remote-desktop viewer. There is no DrissionPage or Playwright CLI fallback.

Discovery only imports the installed SDK; it does not download or start a browser. Preinstall the CloakBrowser binary in the SDK cache or set `CLOAKBROWSER_BINARY_PATH` to that binary. Missing binary/display prerequisites fail explicitly. Automatic updates are disabled. Old `TRACE_BROWSER_EXECUTABLE`, `PLAYWRIGHT_MCP_EXECUTABLE_PATH` and `REA_BROWSER_EXECUTABLE` values are not browser fallbacks. The SDK's internal Playwright dependency is required and remains installed.

Requests use Trace's existing JSONL worker timeout and cancellation path. `cleanup=completed` reports observed local cleanup, not proof that a submitted web operation was rolled back. Linux cancellation attempts context/browser closure and checks owned child processes and private temporary profile deletion; forced termination can still leave the outcome uncertain. Installation and request examples: `examples/cloakbrowser.md`.

For explicit in-place HTML replacement use `set_document(args['html'], wait_until='load', ready_selector='#ready', timeout=10000)`. This optional helper writes once using `page.evaluate`, preserves the current URL (a script changing it raises), then waits for `document.readyState` and an optional CSS selector. `domcontentloaded` waits for a non-loading readyState; `load` waits for complete. The positive timeout is at most 30000 ms and bounds the remaining readiness wait, not arbitrary JavaScript execution; Trace's outer worker timeout/cancellation bounds the action. HTML remains a literal API argument but embedded scripts execute in the current origin. Use only when replacing that document is intended.

CloakBrowser 0.6.4's fixed binary suppresses console/pageerror automation events by design (upstream issue #360, maintainer clarification 2026-09-14). Empty event arrays do not prove absence of logs or page errors. Native `page.set_content` waits for an internal console tag and can time out despite changing DOM; it remains untouched. `set_document` is an explicit separate operation, not an automatic fallback. Exceptions propagate, partial effects remain uncertain, and replacement/submission is never automatically retried. A data URL navigation changes URL/origin and is not equivalent. A selector/readyState proves only the requested observation, not application success or external resource availability.

Real website navigation, forms, login and JavaScript use browser tools first. When a browser is unavailable or unsuitable for a raw HTTP/API request or download, use `http-request` / `page-fetch`, Python `curl_cffi.requests`, or the official `curl-cffi` CLI (`curl-cffi get URL --impersonate chrome`). Managed `curl` delegates to this CLI and uses its grammar; native curl flags are not compatible. Do not bypass the configured transport using native curl or switch client after an uncertain submission.
