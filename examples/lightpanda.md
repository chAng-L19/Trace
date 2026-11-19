# Lightpanda 主浏览器 / CloakBrowser 备用

当前源码默认选择 Lightpanda 做导航、DOM、表单和 JavaScript。Lightpanda 未安装、操作不支持或需要截图/真实图形渲染/原生缩放时选择 CloakBrowser。两者独立工具 ID；已经执行后失败或 uncertain 先核对副作用，不自动重试或跨浏览器重放。

## 冻结依赖

本轮固定 Linux x86_64 官方 nightly 实物：版本 `1.1.0-nightly.12253+166f503a7`，77,989,192 B，SHA256 `bcb4bb1ae80098e9ba7cb47ca69b46dbc8f413176b7987ac5195a392a6cebe3f`。下载来源为 https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-x86_64-linux 。nightly URL 可变，运行/构建不从它解析版本：用已核对的本地文件和 docker/lightpanda.sha256 校验。官方源码基线 `166f503a7b55a287539bf3fd4f18419600c1bb8d`，AGPL-3.0 原文见 licenses/lightpanda-AGPL-3.0.txt。

设置 `LIGHTPANDA_BINARY_PATH=/绝对路径/lightpanda`，或把该冻结文件放到 PATH。Trace 的 Python 环境需已安装 `playwright` 与 `psutil`；这是 CDP 协议和本次进程生命周期依赖，不下载 Chromium，也不启动 Lightpanda 内置 Agent/MCP 循环。工具固定 `LIGHTPANDA_DISABLE_TELEMETRY=true`。发现阶段只核对安装条件，不执行 binary 或启动浏览器；available 不代表已成功访问网站。

## 调用

先 tool_search 精确查询 `lightpanda`，再 tool_select 返回的稳定 ID。每次调用独立 loopback CDP 进程、临时目录及 context，关闭后不保留登录状态；相关步骤放同一调用。Python 原生同步 API 和字面 args：

~~~json
{"code":"page.goto(args['url']); result={'title':page.title(),'text':page.locator('body').inner_text()}","args":{"url":"https://example.com"}}
~~~

输出包括 `backend=lightpanda`、JSON result 及观察到的 cleanup 状态，诊断走 stderr。cleanup 完成不代表撤销网页已提交的操作。超时/取消沿用原 Worker/uncertain 语义，不回退普通 Chromium。

截图、布局和像素相关任务精确查询备用 `cloakbrowser`，沿其 Skill 使用固定 0.6.4 有界面 binary、Xvfb 和现有 set_document 合同。web-analysis 的真实渲染能力与 UI 浏览器验收仍用 CloakBrowser。

## 离线镜像

在指定服务器构建材料中加入 `runtime/lightpanda/lightpanda`（可执行），三个 Dockerfile 都复制并使用 `docker/lightpanda.sha256` 校验。加入 lightpanda Skill 和许可；不从旧基底推断已包含新 binary。不联网下载依赖或自动更新。镜像是否已构建/发布以及本轮实际验收范围，以 DEPLOYMENT.md 为准。
