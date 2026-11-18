# Trace Web Analysis

REA 6.4.0 的 JavaScript/web/Electron 实现子集，作为 Trace 现有 JSONL 本地工具运行。
来源：<https://github.com/morluto/rea>，commit `a2d8b94dd50dab8d966239dd63ab84718aa8b4b4`。
`LICENSE` 保留 REA 原 MIT 许可；`UPSTREAM.json` 逐文件记录来源 SHA-256 与适配标记。
浏览器 SDK 固定为 `cloakbrowser@0.6.4`，参考 CloakHQ/CloakBrowser commit `b1a00eda61efe71da2a1a38831fa6fe861f5d91d`。包装器 MIT 与二进制许可证分别保留在 `CLOAKBROWSER-LICENSE`、`CLOAKBROWSER-BINARY-LICENSE.md`。
这不是 REA 安装器、REA `--scope` 包装或第二个 Agent Runtime。

运行平台为 Linux，Node 22.19+ 或 24.11+。构建、测试只在项目指定服务器执行：

```sh
cd tools/web-analysis
npm ci --ignore-scripts
npm run build
npm test
npm run test:integration
node dist/trace-cli.js --catalog
```

活体验收同样只在服务器执行，显式选择已经安装的引擎：

```sh
CLOAKBROWSER_BINARY_PATH=/absolute/verified-cloakbrowser/chrome xvfb-run -a npm run test:live
CLOAKBROWSER_BINARY_PATH=/absolute/verified-cloakbrowser/chrome TRACE_TEST_ELECTRON=/absolute/electron xvfb-run -a npm run test:live
```

部署阶段通过官方 CloakBrowser SDK 下载及校验二进制，记录实际版本和路径，并将 `CLOAKBROWSER_BINARY_PATH` 固定到该路径。运行阶段只接受该部署路径，不自动下载、不查找系统 Chrome/Chromium、不静默回退。SDK 官方环境变量和许可证逻辑保留；浏览器二进制不是 MIT 许可。Linux 安装 `xvfb` 与 `xauth`，始终使用 `headless:false`。
固定 binary 不透传 console/pageerror 自动化事件（CloakBrowser #360，2026-09-14维护者更正）。场景 launch 仅在选择 console/page-errors 时声明 events 缺失、incomplete 与 equality_eligible:false；不将空日志当无错误。外部 CDP 不凭伪装的 Chrome product 猜引擎，保留 attach-limited 与真实事件/来源过滤，并声明被动 console delivery 未验证。模块解析结果依赖显式页面报告，不依赖 console；diagnostics 明确标注事件缺口，不注入 console shim。原生 set_content 的内部 console tag 等待仍受 binary 限制；Python 工具的显式 set_document 说明见 examples/cloakbrowser.md，不是 JS Page 猴补或原生 API 恢复。
未设置某个引擎变量时，对应测试明确标记 skip。浏览器测试通过真实 CDP 检查脚本、监听器、执行覆盖、请求归因、截图、bundle、场景和原生模块解析；Electron 测试创建临时应用，点击触发 IPC，断言 hook 事件与所属进程清理。

`TRACE_WEB_ANALYSIS_DIR` 指向本目录后，Trace 的 `localtool.Presets` 读取真实操作目录，注册 28 个独立工具及其输入 JSON Schema；目录不在配置中时也检查 Trace 可执行文件旁的 `tools/web-analysis`。依赖缺失会显示为不可用，不自动安装。

直接调用接受每行一个 JSON 对象；一个进程可以处理多行，每个操作结束前关闭该操作资源：

```sh
printf '%s\n' '{"operation":"analyze_javascript_application","input":{"input_path":"/absolute/app-directory"},"output_file":"/absolute/new-result.json"}' | node dist/trace-cli.js
printf '%s\n' '{"input_path":"/absolute/app-directory"}' | node dist/trace-cli.js --operation analyze_javascript_application
```

`input_file` 可代替 `input` 从 JSON 文件读取复杂查询。`output_file` 以排他创建方式保存完整结果，标准输出返回路径、字节数和 SHA-256；现有文件不会被覆盖。失败保留上游错误标签、诊断、部分观测与清理状态，退出码为 1。
`--operation` 接口一次读取完整标准输入，支持 Trace 传入的多行格式化 JSON；不带该选项时按 JSONL 处理。最终资源关闭失败会保留已完成的操作结果；文件发布失败也会在 `partialResult` 中保留操作结果，避免误将输出故障视为操作未发生。
完整参数由 `--catalog` 返回；场景 `browser` 的启动输入只接受 `{ "mode": "launch" }`，旧 `headless` / `executable_path` 字段明确报错。`connect` 与 CDP 被动观察仍保留，不把已有目标替换成新浏览器。新场景来源身份为 `trace-cloakbrowser-scenario`，旧存档不重写。

| 能力域 | 暴露操作 | 实际来源 |
| --- | --- | --- |
| JS 静态语义 | `analyze_javascript_application`, `trace_javascript_semantics` | `src/domain/javascript` 全部非测试模块；`src/application/javascript/JavaScriptArtifact*`、`JavaScriptSemanticGraph*`；独立语义 worker |
| 应用分析 | `trace_application_feature`, `compare_application_versions`, `compare_javascript_export_shapes`, `compare_source_to_bundle`, `reconcile_javascript_runtime` | 上游应用图、版本差异、导出形状、源码对应及运行对账实现 |
| JS 恢复 | `recover_javascript_sources` | Wakaru provider；需要显式 `REA_WAKARU_COMMAND`，Linux x64 Wakaru ^1.13.0（上游验证 1.14.0）；不自造解混淆算法 |
| 网页和 bundle | `list_browser_targets`, `inspect_web_page`, `analyze_web_bundle`, `observe_web_session`, `discover_webmcp_tools` | 上游 CDP provider、DOM/存储/脚本/worker/网络/source-map 采集与静态 bundle 分析 |
| 浏览器操作和比较 | `capture_browser_scenario`, `compare_web_captures`, `capture_web_screenshot`, `compare_web_screenshots` | CloakBrowser 有头场景、事件/网络采集、PNG 比较；独立截图操作仍观察既有 CDP 目标 |
| 脚本和定位 | `export_web_scripts`, `trace_web_module_imports`, `trace_web_source_location` | 脚本摘要清单、原生 ESM/import-map 解析和隔离 source-map codec；模块解析使用同一 `CLOAKBROWSER_BINARY_PATH` 和有头 CloakBrowser |
| 运行归因 | `observe_web_execution`, `inspect_web_event_listeners` | CDP 精确覆盖、请求发起位置、事件监听器 |
| 网络历史记录 | `inspect_web_network_capture` | HAR 1.2、SAZ ZIP/XML、mitmproxy 离线解码；后者需 `REA_MITMDUMP_COMMAND` 指向 mitmdump 12.2.3 |
| V8 Inspector | `list_javascript_runtime_targets`, `observe_javascript_runtime` | Node/Electron Inspector 目标与运行观测 |
| Electron | `list_electron_targets`, `inspect_electron_page`, `capture_electron_scenario` | CDP 页面观测、Electron 专用宿主启动/操作、IPC/contextBridge hook、进程归属和清理 |

静态引擎保留作用域/绑定、符号值、调用/返回、对象与成员写入、getter、异步/promise、模块互操作、数据影响、子进程边界、资源与 Electron 窗口/preload/IPC/contextBridge 等分析；未知与动态值、解析失败、资源上限仍由上游 coverage/limitations 报告。静态图不宣称实际执行，不将 imported Evidence 自动提升为 Trace 的已验证结论。

边界与必要适配：

- 只接受目录或 ASAR 应用分析，保留嵌套 ASAR；inventory reader 裁除 ZIP 通用容器、DMG、Mach-O reader。其他文件可作为叶子资源被哈希，未获得 native 分析能力。
- 应用图消费入口裁除 managed application graph；没有 Android、managed、native 反编译/调试 provider，没有 REA server/registry/runtime/CLI。
- Electron 的 `.node` 加载关系仍作为 JS 静态边界记录；native handoff 字段只是上游观测格式，不安装或调用对应工具。
- 共享文件和子进程归属模块用于语义 worker、codec、浏览器和 Electron 清理；保留其跨平台源码类型，但不打包 Windows 原生 addon 或 Darwin Swift helper。本子包仅支持 Linux。
- CloakBrowser、Electron 程序、Wakaru、mitmdump 均按需部署；npm 包安装和运行请求不下载这些引擎。CloakBrowser 只替代网页浏览器启动，不替代被分析的 Electron 二进制。
- Electron 启动环境在上游选择项之外保留 `XAUTHORITY`，使 `xvfb-run` 和启用认证的 X11 会话可读取所选显示的 cookie 文件；不关闭 X11 认证。
- Linux Electron 的 zygote 可能清空环境，renderer 可能拒绝 `/proc/PID/environ` 读取。仅在本次启动已通过真实 token 验证后，capture-local adapter 以两次稳定进程表、完整父子链、UID、PGID、`/proc/PID/exe` 和 starttime 归属同一 Electron 的后代；清理前重新验证已捕获身份。PID 复用、不同 token、无关同组进程与启动前已存在进程不会取得该归属；清理阶段不接纳新出现的无 token 进程，其他 provider 仍使用原 token 路径。
- 被动 CDP/Inspector 依赖已打开的调试端点；浏览器场景仅启动部署冻结的 CloakBrowser，Electron 主动场景仍显式选择被分析的 Electron 可执行程序。
- Linux 无 `DISPLAY` 时，CLI 为实际浏览器启动请求创建一次 `xvfb-run -a` 显示持有进程；原 Node CLI 始终保留为 Worker 根进程，接收 SIGTERM/SIGINT 并取消当前操作。浏览器清理完成后关闭持有进程 stdin，再等待 Xvfb 退出，显示清理失败保留错误及原操作结果。catalog/静态操作/场景 connect 不启动显示服务；命名调用和 JSONL 使用同一边界。直接调用模块时由调用方使用 `xvfb-run -a`。不新增常驻服务。
- 强杀根进程或 SIGKILL 仍不证明浏览器与 Xvfb 已清理，保留 cleanup unknown 边界。
- SIGINT/SIGTERM 会传递到操作的 AbortSignal 并关闭输入读取，当前操作清理及结果输出结束后分别以 130/143 退出，不再等待或执行下一条请求。已请求的 `output_file` 仍保存该操作返回的完整成功/失败/部分观测，写入不复用已取消信号，避免丢失清理证据。SIGKILL 或宿主崩溃绕过 JavaScript 清理，独立子进程与临时目录的清理状态应视为未知，不能由外层进程结束推断已清理。

直接 npm 依赖均固定版本。`playwright-core@1.58.2` 保留为 CloakBrowser SDK 的协议依赖、现有 CDP 连接与 Electron 专用宿主，不暴露独立 Playwright 浏览器启动入口，不宣称零 Playwright 依赖。Babel 解析器/类型、Zod、ASAR、trace-mapping、parse5、CloakBrowser、ws、ZIP/XML/HAR/AJV、canonicalize、jsonc-parser、lossless-json、semver 是选定子集的实际依赖；没有 MCP SDK、REA UI、PTY、Git 或 native 分析引擎依赖。各包许可证由安装包自带保留。

回归：移植 57 个上游测试文件，涵盖静态语义、Electron 静态边界、图/版本/源码映射、HAR、脚本捕获与 PNG 比较；另有 JSONL 目录/真实 worker/错误/防覆盖集成测试。活体浏览器与 Electron 验收需要服务器可执行文件和测试应用；单元测试通过不等于该活体能力已验收。
