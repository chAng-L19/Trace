# CloakBrowser 本地浏览器工具

Trace 用 `cloakbrowser==0.6.4` 官方 Python SDK 替换旧 DrissionPage / Playwright CLI 入口。SDK 返回原生同步 Playwright 对象，其 `playwright` Python 依赖仍需保留；无需安装 `@playwright/cli` 或启动浏览器 MCP。

## 安装与发现

在 Trace 使用的 Python 环境中安装 `cloakbrowser==0.6.4` 及 `psutil`（发行环境固定版本见 `docker/Dockerfile`）。浏览器二进制由管理员在安装阶段预先准备：

~~~sh
python -c 'from cloakbrowser import ensure_binary; print(ensure_binary())'
~~~

该命令可能下载上游 Chromium；Trace 的工具发现和浏览器动作不会调用下载流程。默认使用 SDK 已安装的缓存路径，也可显式设置 `CLOAKBROWSER_BINARY_PATH=/absolute/path/to/cloakbrowser/chrome`。不将系统 Chromium 或旧浏览器环境变量当作备用。

Linux 无物理显示时安装 `xvfb`、`xauth`；适配器通过 `xvfb-run -a` 为本次动作创建独立虚拟显示。已有 `DISPLAY` 时使用该显示。两种路径均是 `launch(headless=False)`，Xvfb 不等于 headless，也不提供远程桌面。SDK 缺失、二进制缺失、显示失败均显式报错，不回退旧工具。发现可用只证明 Python 依赖可导入，浏览器/显示在动作执行时检查。

## 调用

先 `tool_search` 搜索 `cloakbrowser`，再 `tool_select` 激活返回的稳定 ID。输入是 `code` 与字面 `args`，不是 shell 或 CLI argv：

~~~json
{
  "code": "page.goto(args['url']); result={'title':page.title(),'text':page.locator('body').inner_text()}",
  "args": {"url": "https://example.com"}
}
~~~

表单操作与截图示例，字段定位以实际页面为准：

~~~json
{
  "code": "page.goto(args['url']); page.get_by_label('Search').fill(args['query']); page.get_by_role('button', name='Search').click(); page.screenshot(path=args['screenshot'], full_page=True); result={'url':page.url,'title':page.title(),'screenshot':args['screenshot']}",
  "args": {"url": "https://example.com/search", "query": "literal {{run_id}} $(text)", "screenshot": "/absolute/run/workspace/browser.png"}
}
~~~

截图完成后用 `artifact_register` 登记文件。`page`、`context`、`browser` 已创建；结果放在 `result`，`print()` 进入诊断 stderr，stdout 只返回一条 JSONL 结果。每调用独立 browser/context，不跨调用保留登录状态或 localStorage；相关步骤需在同一次调用完成。超时、取消、脚本/序列化异常进入现有 Worker 失败路径并尝试清理，提交副作用不自动重放。

## 验证（仅服务器隔离源码）

### 原位 HTML 文档替换

固定 CloakBrowser 0.6.4 的原生 `page.set_content` 已观察到 DOM 改变后仍超时；保留原方法和异常，不将超时视为成功，也不自动换 data URL 重试。需要保持当前 URL 的明确文档替换，可显式调用：

上游 issue #360 的维护者于2026-09-14确认：binary 设计上不透传 console/pageerror 自动化事件，撤回早期修复承诺。Playwright 原生 `set_content` 等待 utility-world 的 console.debug tag，故 DOM/load 已发生仍超时；不是缺失浏览器安装或等待时间过短。空日志不证明页面无错误。Trace 场景 launch 选择这些事件时标记 events 缺失、不具完整性比较资格；外部 CDP 只声明被动采集未验证 delivery，不猜测 binary 身份，不主动注入日志 shim。官方说明：https://github.com/CloakHQ/CloakBrowser/issues/360#issuecomment-5658501264 。固定SDK/binary保持不变。

~~~python
page.goto(args['url'])
observation = set_document(args['html'], wait_until='domcontentloaded', ready_selector='#ready', timeout=10000)
result = {'document': observation, 'title': page.title()}
~~~

helper 单次 `evaluate` 执行 `document.open/write/close`，HTML 作为参数传递，不拼接脚本；随后显式等 readyState 和可选 CSS 选择器。它不修改 Playwright 全局或原生 `set_content`，也不承诺等价生命周期事件。HTML 内脚本在原 origin 执行；脚本导致 URL 改变将报错。`timeout` 必须为 0–30000 ms 范围内正数，只约束剩余就绪等待；任意 JavaScript 写执行仍由外层 Worker 动作超时/取消约束。等待、选择器或 URL 错误直接失败，已经发生的 DOM/脚本效果不回滚，不重放。

单元契约：`python -m unittest discover -s internal/localtool -p browser_document_test.py`。真实 headed 验收：`TRACE_CLOAKBROWSER_TEST=1 go test ./internal/localtool -run TestCloakBrowserSetDocument -v -count=1`，覆盖 URL/DOM/click、就绪超时、非法选择器、脚本改 URL，均检查清理。两类结果分别记录，单元 stub 不代替浏览器实测。

~~~sh
go test ./internal/localtool ./cmd/trace
TRACE_CLOAKBROWSER_TEST=1 go test ./internal/localtool -run TestCloakBrowser -v -count=1
~~~

第一项包含 SDK stub 的 headed 参数、字面输入、stderr、启动/脚本/序列化/关闭失败及无下载负控；第二项启用真实浏览器导航/填写/点击/截图、同 Run 与跨 Run 存储隔离、取消后进程与私有目录清理检查。无 `TRACE_CLOAKBROWSER_TEST=1` 时只跳过真实浏览器用例，不以跳过作为浏览器验收。

参考版本：CloakHQ/CloakBrowser `b1a00eda61efe71da2a1a38831fa6fe861f5d91d`，Python SDK `0.6.4`，MIT；调用契约来自 `cloakbrowser/browser.py` 的 `launch`/`close`，缓存观察来自 `cloakbrowser/download.py` 的 `binary_info`。Trace 不复制其浏览器引擎实现。
