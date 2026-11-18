# Trace CLI

Trace 是以持久 Run 为核心的研究 Agent，CLI 与 WebUI 共用 Go Agent、Store、执行账本和工具目录；pi-agent-core/pi-ai 1.4.0 是唯一模型—工具循环。Go宿主要求Go1.22+，Node>=22.19；Python、浏览器与分析工具按需安装。根 `agent.md` 是维护者约定，`prompts/agent.md` 才嵌入运行时模型。

## 当前入口

- 源码位于 `E:/cli/trace`；当前正式版本为 `optimize-pi-20261117`；此前运行时和多模型配置已发布，本轮工作台固定滚动/模型编辑区改版已在服务器隔离验收，尚未发布。已登记的完整TSB镜像为 `trace:full-agent-tsb-20261112`，尚未同步后续修复；发行身份、完整包和旧版清理边界见 [DEPLOYMENT.md](DEPLOYMENT.md)。
- [HANDOVER.md](HANDOVER.md) 是短交接，[开发计划.md](开发计划.md) 是唯一当前待办/待吸收清单，[FEATURE_COMPARISON_CURRENT.md](FEATURE_COMPARISON_CURRENT.md) 保留参考来源与采用理由。
- CloakBrowser0.6.4固定binary/headed/Xvfb保持；11月6日P0/cache/T3/D1/UI/Skill与最后浏览器能力合同修补均已正式部署。显式set_document可用，原生set_content和console/pageerror仍为固定binary限制，空事件不证明无日志/无页面错误。
- TSecBench 历史复盘见维护文档，本批未操作比赛或实例。T2原B9三页面500ms门槛仍FAIL，缓存实际命中率/TTFT/成本及模型策略收益待实测；合成链和部署通过不代替平台成绩。
- Muteki 当前固定参考源码为 `a73535a19fd2c1cddd97dfbeea65f62f4ce136ee`（`E:/cli/_refs/muteki`）；其他固定参考见对照文档。pi实际依赖不是换成pi CLI。除pi core外，不迁入参考仓库作为第二运行引擎或CTF控制体系。
## 阅读导航

模型检测：编辑区检测按钮下方显示当前配置的短请求结果、耗时和检测时间，保存回执也在编辑区；右上角仅补充显示当前默认连接状态。当前标签页内切页及同值配置重读保留。未保存表单的检测明确标“草稿、未保存”，检测后保存同一配置会保留结果；保存不同未检测模型会回到未检测。结果仅存内存，刷新页面后需主动检测，不自动发请求或存储凭据。

MCP 管理：左侧“MCP 管理”→“添加 MCP 服务”，直接打开唯一添加弹窗，选择连接已有服务或从 GitHub 下载源码并接入；页面不再保留添加/编辑折叠表单。保存成功关闭弹窗，取消/Escape/关闭丢弃本次输入；已有服务从详情进入编辑。启用/停用位于每张服务卡片的开关与服务详情顶部；停用保留配置、关闭 Trace 持有的连接并移出工具目录，HTTP 远端服务进程不受影响。检测连接只是临时探测，不改变启用状态。变更前需暂停运行与 Worker；重名新增会提示编辑原服务，旧连接/JSON草稿遇到配置更新会阻止覆盖，可复制草稿后丢弃并按最新配置重编。

- [系统架构](#系统架构)：模块职责、执行闭环、协作、存储与实时界面。
- [构建与运行](#构建与运行) · [命令](#命令) · [已集成工具](#已集成工具)。
- [本地工具 manifest](#本地工具-manifest) · [MCP 与 Worker](#mcp-与-worker) · [GitHub 接入与更新](#github-mcp--skill-接入与更新)。
- [Provider](#provider-前提) · [上下文与 Skills](#上下文会话与-skills) · [按需工具 schema](#按需工具-schema)。
- [文件与 CAS](#文件与-cas) · [WebUI](#本地-webui) · [预算](#预算与用量) · [结果与证据](#结果与证据) · [已知边界](#已知边界)。
- 维护资料：[维护约定](agent.md)、[交接摘要](HANDOVER.md)、[开发计划/当前状态](开发计划.md)、[部署说明/验收历史](DEPLOYMENT.md)、[参考项目对照](FEATURE_COMPARISON_CURRENT.md)。

## 系统架构

以下描述本地源码；线上发行版与验收记录见 [DEPLOYMENT.md](DEPLOYMENT.md)，待发布改动见 [开发计划.md](开发计划.md)。

CLI 与 `serve` 共用 Go Agent、Store、ToolRegistry 和执行账本；`serve` 在同进程提供 HTTP API、SSE、嵌入式静态页面。不同 CLI/服务进程使用同一数据目录时以 OS 文件锁协调。模型提出调用和结论，Runtime 管理生命周期、预算、执行与持久化，Store 是权威来源。

WebUI为原生ES模块。嵌入的现用JS/CSS/HTML/SVG在服务启动首次访问时用标准库预压缩，按Accept-Encoding返回gzip；以内容ETag和`private, no-cache`逐次重新验证，避免刷新后沿用旧模块。API、SSE、用户产物保持原缓存策略，运行数据不进入静态缓存。旧页面备份与废弃样式仍留源码，但不嵌入发布二进制。

| 模块 | 职责与边界 | 主要源码 |
| --- | --- | --- |
| CLI / 服务启动 | 解析命令、数据目录、环境配置，构建 Agent；`serve` 加载服务配置并启动 HTTP | [cmd/trace/main.go](cmd/trace/main.go)、[serve.go](cmd/trace/serve.go)、[service_config.go](cmd/trace/service_config.go) |
| Agent / pi core | pi runAgentLoop调度模型与工具轮次；Go保留Run锁、预算、事务、暂停恢复和完成Judge | [agent.go](internal/agent/agent.go)、[pi_loop.go](internal/agent/pi_loop.go)、[host.mjs](internal/picore/host.mjs)、[bridge.go](internal/picore/bridge.go)、[tools.go](internal/agent/tools.go) |
| Provider | OpenAI-compatible 请求、流式正文/思考/调用参数、usage；Mock 用于无外部模型的状态检查 | [provider.go](internal/agent/provider.go)、[stream_batch.go](internal/agent/stream_batch.go) |
| 上下文 | 固定提示词、消息投影、历史摘要、工具 schema 选择、Skill 目录与 Worker 来源清单 | [context.go](internal/agent/context.go)、[context_semantic.go](internal/agent/context_semantic.go)、[tool_projection.go](internal/agent/tool_projection.go)、[skills.go](internal/agent/skills.go) |
| 工具目录与适配 | 稳定工具 ID、schema、适配器及真实执行身份；按类型路由调用 | [registry.go](internal/agent/registry.go)、[internal/localtool](internal/localtool)、[internal/mcp](internal/mcp) |
| 进程执行器 | exec / localtool 子进程、输出暂存、PID、退出/取消与结果不确定状态 | [internal/worker](internal/worker) |
| Planner / Agent Worker | 持久 DAG、就绪前沿、容量、租约、child Run、协作消息与原 child 恢复 | [planner.go](internal/agent/planner.go)、[agent_workers.go](internal/agent/agent_workers.go)、[worker_resume.go](internal/agent/worker_resume.go) |
| Store / CAS | 默认原子JSON；显式迁移后单SQLite WAL/FULL，跨进程锁、原始字节归档、删除与中断恢复 | [store.go](internal/agent/store.go)、[store_views.go](internal/agent/store_views.go)、[artifacts.go](internal/agent/artifacts.go)、[store_purge.go](internal/agent/store_purge.go) |
| 证据与验收 | 来源交叉校验、有序证据链及固定引用、要求检查、发现审阅与新执行复测 | [report_evidence.go](internal/agent/report_evidence.go)、[evidence_chains.go](internal/agent/evidence_chains.go)、[verification_tools.go](internal/agent/verification_tools.go) |
| Web 后端 | 控制 API、配置重建、SSE、workspace 投影及按需历史读取 | [server.go](internal/web/server.go)、[workspace.go](internal/web/workspace.go)、[config.go](internal/web/config.go) |
| Web 前端 | 原生 ES modules、统一对话、探索图、证据管理、增量渲染与视图状态 | [app.js](internal/web/static/app.js)、[workspace.js](internal/web/static/workspace.js)、[interactions.js](internal/web/static/interactions.js) |

### 执行、恢复与 Worker

创建 Run 时保存目标、提示词及 hash、预算与附件。每轮交付持久指令/协作消息，投影上下文与 schema，**先保存 ModelRequest，再调用 Provider**；流式预览合批落账，完整响应归档后才执行完整合法的工具参数。工具路径为 `Action started/原参数 → 适配器执行 → ToolResult/tool-role 消息 → Evidence/CAS`。模型结束回复还需通过要求、谓词、计划和 Worker 检查。

- 状态：`created/running/waiting_tool/paused/completed/cancelled/failed`；暂停原因包括预算、等待输入、上下文超限、动作结果不确定。恢复重新检查门槛，不无条件继续。
- 执行后、提交结果前中断的副作用保留 `uncertain`，不自动重放；未知工具保持 pending；人工回填为 `unverified`。`completed`、业务 `success`、来源 `verified` 分开。
- 浏览器断开仅结束订阅；服务关闭先暂停运行、取消并等待 Worker，再关闭 MCP。
- Planner 是持久 DAG，以依赖/frontier、priority、capacity、lease 派发同进程独立 child Run；每个 child 有自己的Run记录（默认JSON，迁移后同一SQLite内独立Run行）、锁、消息、请求、动作、证据和 MCP 会话。它不同于 `internal/worker.Run` 启动的外部工具进程。
- 默认每根会话整树容量5，根计划可显式调整，全部嵌套 Worker 共享；独立会话及普通 Branch 分开计算。child 只自动接收声明的 `AssetIDs`、直接完成依赖和显式附件，来源保留 Run、Evidence/hash、CAS；作为 user/data 每次重验，不复制父历史或冒充 child 执行证据。固定上下文、schema、输出预留及来源清单共同受预算约束。
- `worker_status/message/wait/cancel/recover` 管理协作；恢复沿用原 child，父结果受依赖与清理状态约束。当前无远程集群、容器权限隔离或常驻 dispatcher；child 上下文隔离不等于 OS 隔离。
- P2的`resource_keys`约束同worker树显式共享写资源；`required_tool_ids/required_asset_ids`在Claim前检查，`expected_artifacts/requirements`进入child。局部缺能力可让其他独立task继续，Store真错仍上送；紧耦合跨文件任务不强拆。只读builtin白名单最多5路工具RPC，mixed/exec/edit/MCP仍默认顺序，结果先持久化再按声明顺序结算。

### exec 资源域

Linux exec 默认 `TRACE_EXEC_RESOURCE_MODE=cgroup`，必须位于新建的 cgroup v2 委托 unit：原子进入逐 call leaf，缺 controller/delegation/clone3 支持时返回 not_started，不自动切换后端。scripts/exec-cgroup-isolated.sh 仅用于新隔离 unit，拒绝复用已有正式服务域。默认 controller 1GiB/128 tasks、exec aggregate 1.5GiB/384 tasks，父 unit 至少 2816MiB/576 tasks 且 swap=0；这只是域内预留，不保证无关主机负载不触发全局 OOM。

严格模式每 call 默认 256MiB/64 tasks、stdout+stderr 合计8MiB，可由 operator 设置 TRACE_EXEC_MEMORY_BYTES、TRACE_EXEC_PIDS、TRACE_EXEC_OUTPUT_BYTES。TRACE_EXEC_CGROUP_ROOT 由委托入口导出；准入保守计入 aggregate 当前占用、现存 leaf 额度与64MiB/8 tasks余量，资源不足显式 not_started，不承诺任意内存状态下恒有五个 exec 同时启动。资源限制不构成子 Agent 求解限时。

托管镜像显式设置 `TRACE_EXEC_RESOURCE_MODE=process-group`，使用独立进程组锚点、继承 owner 标记与 pidfd 清理，保留共享输出上限；没有逐 call 内存/PID 硬隔离。要求独立 PID namespace、pidfd 可用及同 UID `/proc` 可读，读取失败则阻断。清空标记并脱离原组、改变 UID 或隐藏进程超出其清理边界；容器整体限额由平台提供。详见 docker/README.zh-CN.md。

started 事务固定 owner、后端和资源身份：cgroup 使用原根 dev:inode，process-group 绑定 boot/PID namespace/UID。恢复按保存的后端只清理、不重放，保留 uncertain；当前 ENV 不改写旧归属。换根/根消失或重建容器后 namespace 改变不当作清理成功，手工业务回执不替代进程清理证明。该限制仅覆盖 exec，不把 local/pi/常驻 browser 冒称逐 call 隔离。

### 存储、证据与上下文

默认Run JSON同时保存快照与账本：资产/关系、消息、请求响应、动作、事件、Plan/Worker、信箱、压缩记录、证据链、验收/发现及结果。CAS 按 SHA-256 保存原始字节；列表/事件/观察缓存可重建，不是第二份权威记录。

```text
TRACE_DATA_DIR/                 # 默认 state；服务 --config 可另选配置路径
  <run-id>.json / <run-id>.lock # 快照与跨进程执行锁
  .store.lock                  # Store变更锁
  artifacts/<sha256>           # 输出、交付文件和附件原始字节
  inputs/<input-id>.json       # 附件元数据，引用CAS
  service/web-config.json      # 可能含凭据，界面脱敏
  service/integrations/        # GitHub导入记录与checkout
```

默认JSON后端在锁内写临时文件、同步并rename替换；事件追加复用已提交历史字节。显式迁移后的SQLite后端见下节；两种模式均不是独立JSONL事件库，也不承诺任意断电下的CAS/数据库跨介质原子事务。证据按 `ModelRequest → Response/tool_calls → Action(request/call、参数、适配器/执行身份) → ToolResult/tool消息 → Evidence/CAS` 交叉核验。证据链固定源 Run/链ID/hash，`derived_from` 保留多来源方法血缘；原版本保留，引用/组合不执行命令，不证明新任务成功，也不是自动重放/通用自学习。

模型输入为运行提示词、历史或有效摘要、Skill目录、计划/验收、Worker来源和选中schema。只有 `prompts/agent.md` 被 embed；根 `agent.md` 不进入请求，外部文件/工具/MCP prompt/resource/经验链仍是数据。默认同 Provider 语义摘要，也可选择确定性 projection；原消息、完整工具配对、Evidence/CAS、摘要请求/usage/失败均保留。估算是保守 wire bytes，不是 tokenizer；固定部分/完整工具批次超限时暂停，不截断配对。

| 类型 | 接入与边界 |
| --- | --- |
| builtin | Agent内执行文件、Planner、证据、Skill等能力，共用账本 |
| exec | Unix `/bin/sh` 或 Windows `cmd.exe`，使用服务账号权限 |
| localtool | 原生argv与每次进程JSONL stdin，按需Python/Node/helper；不是MCP/第二Runtime |
| MCP | stdio/HTTP，以稳定ID和远端身份路由；child独立会话 |
| Skill | 发现元数据，按需读正文；禁用只影响后续模型可用目录，管理端仍可读 |

小目录直接提供schema，大目录通过 `tool_search/tool_select` 加载并持久化。CLI/Web共用完整服务配置；`TRACE_CONFIG_FILE` 指定文件，默认 `TRACE_DATA_DIR/service/web-config.json`，serve的`--config`可单独指定。保存文件整体优先；仅缺文件时用Provider/Context/Skill/MCP/localtool等env引导，损坏或null明确报错，不静默回落。运行预算/成功谓词仍按各Run设置；resume使用原Run提示词，env-only CLI未显式设Context时保留原值。目录命令只发现工具，不启动Run或解析运行预算。仅模型连接、命名配置、角色默认或active_provider变更时，原子保存后发布独立模型路由副本，保留Store、工具与MCP连接；活动请求继续原绑定。其他配置更新仍先建候选Agent，失败保留旧实例，主任务/Worker活动时不替换。密钥不进入模型请求账本，服务配置本身可能含凭据。

### SQLite 存储维护（T2，显式启用）

默认新建/既有数据目录继续使用原子JSON，不随升级自动迁移。候选后端仍是原Go Store，固定纯Go驱动 `modernc.org/sqlite v1.34.5`，无需CGO或独立数据库服务；单 `store.sqlite` 使用WAL、每连接 `synchronous=FULL`。Run小状态、Worker操作receipt与事件变更同事务，成功提交后才通知SSE；事件按Run/sequence索引补读。执行入口仍完整核验证据、uncertain不重放和整树预算，不引入第二权威Store。CLI/serve退出释放Store连接池；嵌入式调用者应在所有消费者停止后调用 `Store.Close()`。

以下命令是维护入口示例，不会由启动/升级自动执行。**先停止该数据目录的全部服务、CLI执行owner与Worker并确认清理完成，再由维护者显式执行**；命令会取得Store排他锁和各Run锁。迁移前应另外保存完整停写备份，包含CAS、inputs及所需配置。不要把旧版本二进制继续指向迁移后的目录。

```bash
TRACE_DATA_DIR=/absolute/path/to/stopped-state trace store-migrate sqlite
TRACE_DATA_DIR=/absolute/path/to/stopped-state trace store-export /absolute/path/to/new-json-snapshots
```

- 迁移先校验Run身份与事件序号：必须为正、严格递增且不超EventSeq。乱序、重复或非法历史明确拒绝，原JSON保持，不静默重排。导入完整临时DB并同步后，原子发布 `.sqlite-authority`（指向已同步候选）与 `store.sqlite`；marker后中断由下次打开完成发布与归档。
- 原Run JSON移到 `sqlite-migration-json-backup/` 并设只读，仅表示**迁移时刻**，不会双写或跟随后续提交。DB成为唯一权威；DB和候选同时缺失、损坏或版本不匹配都报错，绝不静默回退旧JSON。已打开的旧JSON Store拒绝继续写。不得删除marker/DB来“回滚”，否则可能丢失迁移后的事件、receipt或执行状态。
- `store-export` 仅向**不存在的新目录**导出当前JSON快照，不切换后端，不覆盖已有目录；中途失败可能留下不完整目录，必须核对后另选新目录重试。**它不是完整备份**：不复制 `artifacts/`、`inputs/`、服务配置/密钥、集成checkout或外部workspace；JSON中的CAS/附件引用原样保留，单独导出目录的引用可能不可用，缺失或变更的来源仍会被Judge撤销验证状态。
- 若需退出SQLite模式，保持全部owner停写：导出到新目录，另行复制并核对同一停写时刻的CAS/inputs及实际所需配置/工作文件，检查原字节SHA-256、来源校验、Run/receipt/event数量和预算状态；再由维护者明确切换数据目录并验证。不复制旧执行锁持有状态，不自动恢复运行，不把迁移时备份覆盖当前DB；正式服务配置切换/重启与回滚均需独立操作指令。保留原SQLite目录用于核对，打开的WAL数据库不以单独复制 `store.sqlite` 充当一致备份。

该后端的功能回归与B9性能结论分别记录于 `DEPLOYMENT.md`。SQLite启用不等于原五Worker、3×50k事件、三页面source→DOM p95≤500ms目标已经达标；不改变验收门槛。Windows目录同步/实际迁移未据Linux回执宣称已验收。

### 实时界面

`Provider delta/MCP progress/工具输出 → 有界合批 → Store EventSeq → SSE游标 → live.js投影 → conversation.js/探索图`。首次读取轻量 workspace，完整请求和原始事件按需读取；`Last-Event-ID`/`after` 补读，按 Run/request/call 去重，避免逐token全页刷新。

实时轨迹已移除；统一对话保留正文、思考、工具调用/返回和进度，调用详情从工具、待核对调用或文件来源打开。请求/schema按需读取，原始事件及探索图历史回放保留。

Provider约200ms合批并在结束drain；SSE约150ms检查、15s心跳。这是实现节奏，不是端到端承诺；持久化失败反馈调用路径，预览不代替完整响应或执行结果。`interactions.js` 保留草稿/滚动/展开。探索图投影真实资产、关系、计划和执行，不另行调度或验证。Go服务定位单用户，公网访问由nginx Basic管理。

## 构建与运行

当前项目维护约定：产品构建、测试和浏览器验收在部署服务器执行；本机仅编辑和静态阅读。下面的命令是通用使用示例。

```powershell
# Node >=22.19；固定安装，不在请求路径动态安装依赖
npm ci --prefix internal/picore --omit=dev --ignore-scripts
$env:TRACE_PI_RUNTIME = (Resolve-Path internal/picore).Path
go test ./...
go vet ./...
go build -buildvcs=false -o trace.exe ./cmd/trace
$env:TRACE_DATA_DIR = "E:\trace-state"
.\trace.exe create "目标描述"
.\trace.exe run RUN_ID
.\trace.exe result RUN_ID
.\trace.exe evidence RUN_ID
.\trace.exe report RUN_ID report.md
```

未配置 Provider 时使用 Mock。Mock 可检验 CLI/持久状态，但不产生真实工具或目标成功证据。默认数据目录是当前工作目录下的 `state`；跨目录调用时应设置固定的 `TRACE_DATA_DIR`。

## 命令

| 命令 | 用途 |
| --- | --- |
| create GOAL | 建立运行并保存提示词、预算与成功谓词 |
| serve [--addr 127.0.0.1:8765] [--config FILE] | 启动本地 WebUI、HTTP API 与 SSE |
| run / resume RUN_ID | 执行或继续；不会绕过取消状态或耗尽预算 |
| message RUN_ID MESSAGE | 给非活动、非暂停、无待工具结果的运行追加指令 |
| tool RUN_ID CALL_ID RESULT | 给明确 pending ID 回填观测；该观测不是已验证执行证据 |
| tool RUN_ID CALL_ID --exec COMMAND ARGS... | 实际执行本地进程并保存结构化结果 |
| tool RUN_ID CALL_ID --mcp TOOL_ID | 按稳定 ID 执行匹配的 pending MCP 调用，使用持久保存的原参数 |
| tool RUN_ID CALL_ID --registered TOOL_ID | 执行匹配的本地 manifest 或 MCP 工具，使用持久保存的原参数 |
| tools | 显示全部内置、本地 manifest 与 MCP 工具的稳定 ID、适配器、原名与 schema |
| mcp | 仅显示外部接入目录（local/MCP），排除 exec 与其他 builtin；不是 Trace MCP server |
| pause / cancel RUN_ID | 可从另一个 CLI 进程中断活跃运行 |
| show / events / list | 读取持久快照、事件和运行列表 |
| result / evidence RUN_ID | 读取重验后的结果或证据账本 |
| report RUN_ID [OUTPUT] | Markdown 报告；省略路径输出 stdout，已有文件不覆盖 |
| prompt / version / help | 查看新运行提示词哈希、版本和命令帮助 |

内置工具 `exec` 参数为 `{"command":"shell command"}`；Windows 用 cmd.exe，Unix 用 /bin/sh。运行时还会自动发现并注册下述可用外部工具 preset。只有执行结果已保存才继续模型。未知工具保持 pending。崩溃时有 started 无结果的动作保持 uncertain，不自动重放；先核对实际副作用，再恢复到 waiting_tool，按原 call ID 回填结果。

## 已集成工具

`run/resume/message/tool/tools/mcp` 自动发现Python3.9+与可选依赖；小目录直接给schema，大目录按需 `tool_search/tool_select`。helper嵌入Go二进制，经原生argv+JSONL执行，不依赖源码cwd、不自动安装依赖。缺依赖的preset不注册，以 `trace tools/mcp` 为准。

| 依赖 | 工具与边界 |
| --- | --- |
| Python标准库 | http-request、page-fetch、dns-resolve、port-probe、binary-info、strings、analysis、code-search、python-ast-audit；analysis仅元数据/hash/字符串，AST为启发式。字符串/源码读取在内存，HTTP/匹配结果显式标截断 |
| Capstone | disassemble：指定架构/文件偏移的原始字节反汇编 |
| Frida SDK | frida：devices/processes/instrument；PID/进程名+任意JS，结束卸载/脱离；不建持久会话/不spawn，脚本/清理错误非零，取消保留uncertain |
| Droid ASC | droidasc：APK类/Manifest、字符串/类型/方法/字段引用、按需单类反编译；Python CLI，不计MCP |
| PATH中Rizin/radare2/r2 | radare2，按需安装 |
| CloakBrowser 0.6.4 | 唯一通用浏览器入口，官方Python SDK：导航/DOM/填写/点击/JS/截图；每次调用独立有界面浏览器并关闭；无桌面Linux由Xvfb渲染 |
| REA选定子集 | web/JavaScript/Electron 28操作；既有localtool，不恢复整个REA/IDA/native/managed/Android Runtime |

HTTP非2xx是观测，exit0不代表目标成功。HTTP/page默认验证TLS；单次 `verify_tls:false` 返回 `tls_verified:false`，HTTP为null，不自动重试或改全局信任。服务配置保存完整helper/schema，升级二进制不会刷新旧预设，须另同步。

- **Frida**：实测SDK17.17.0，Trace环境 `python -m pip install frida`；[示例](examples/frida-local.windows.json)、[Skill](.trace/skills/frida/SKILL.md)。
- **ASC**：[Droid ASC](https://github.com/MG1937/ASC)，本机editable `droidasc==0.1.1.post5`、`E:/cli/_refs/ASC`、基线 `d3d75ab7a3c092345102d2ec66ab614ad4a6cc75`。调用 `python -m droidasc`；其他主机在Trace环境 `python -m pip install --editable /path/ASC`，源码更新直接生效、依赖另装。合成APK已验发现→选择→Skill→类/引用/反编译→Action/Evidence，不代表任意APK全路径；[示例](examples/asc-local.windows.json)、[Skill](.trace/skills/asc/SKILL.md)。
- **REA**：移植固定MIT版本JS静态语义/web/CDP/浏览器/Electron，现有localtool接入；旧3个REA JSON字段只验历史证据，旧CAS仍可读。操作、安装、来源/许可和Linux边界见 [tools/web-analysis/README.md](tools/web-analysis/README.md)。
- **浏览器**：[CloakBrowser](https://github.com/CloakHQ/CloakBrowser)，固定 SDK `cloakbrowser==0.6.4` 与 `psutil==7.2.2`，部署阶段安装并通过上游签名校验下载官方浏览器，再设置 `CLOAKBROWSER_BINARY_PATH` 到冻结可执行文件；`CLOAKBROWSER_AUTO_UPDATE=false`。现有 localtool→JSONL→Worker 接入；输入 `code`/字面 `args`，使用同步 `page/context/browser` API，结果赋给 `result`。每调用独立有界面浏览器，Linux 缺 DISPLAY 时自动使用 Xvfb/xauth；无远程桌面或跨调用登录持久化。错误不回退旧工具，不重放不确定提交。SDK 内部 Python Playwright/JS playwright-core 是协议依赖，独立 Playwright CLI 与 DrissionPage 已移除。使用前 `trace tools` 核对；[Skill](.trace/skills/cloakbrowser/SKILL.md)、[示例](examples/cloakbrowser.md)。SDK MIT 与浏览器二进制许可分别保留于 tools/web-analysis。

### 工作区与交付

固定浏览器限制：console/pageerror 事件不透传，空捕获不是无错误证明；原生 `page.set_content` 可能在 DOM 已写后超时。明确原位替换用 `set_document(args['html'], ready_selector='#ready', timeout=10000)`，单次执行、失败不重放；说明与边界见浏览器 Skill/示例。场景采集选择受限事件时标 incomplete，不假装完整。

任务内同区切换对话/轨迹、探索链路、证据/产物/结论/协作/事件；调用详情按需看参数/结果/schema/耗时/原文。右侧指令/Worker/预算/上下文，父子导航保持任务。未发布UI：探索图左列下方/移除重复入口，页签各记滚动；思考进行中展开，正文/工具开始、响应结束/中断后自动折叠，可手动展开。仅留本地，未运行/上传，不代表线上生效。

证据页默认完整操作链：选真实操作、排序、逐步说明，一链一记录、步骤独立展开；跨Run/结论引用保留用途/固定hash，原账本可查。`evidence_chain_search/read/create/reference` 检索/读取/引用；新观察用 `method_id/variant/applicability/lesson/derived_from` 吸收多个来源形成新方法，旧版本/多方法保留，来源失效沿血缘传播。引用/组合不执行命令，不替代新Run验收。

CLI：`trace chains [RUN_ID CHAIN_ID]`、`trace chain-create RUN_ID JSON_FILE`、`trace chain-reference TARGET_RUN SOURCE_RUN CHAIN_ID CHAIN_HASH`。

档案→删除默认永久清除全文/思考/请求响应/事件/证据链、Worker后代与独占CAS；共享上传/workspace/外部备份保留。先暂停等Worker清理；外部引用阻止删除并列来源，先处理引用。仅收起选“仅隐藏”，历史保留；旧隐藏记录在“已隐藏会话”管理/永久删除。

## 本地工具 manifest

一个通用适配器接入现有 CLI 或 Python/helper，无需为每个产品开发 Go adapter。自定义 manifest 追加到自动发现的 preset，同名时使用用户配置；自定义配置内部重复报错。默认二进制嵌入 helper 源码，不打包 rg、Python 或任何云厂商 CLI。

```powershell
# 从项目根目录运行示例；依赖 rg 和 python 已在 PATH 中。
$env:TRACE_LOCAL_TOOLS_CONFIG = "E:/cli/trace/examples/local-tools.json"
.\trace.exe tools
.\trace.exe run RUN_ID
# 对此前缺少配置而 pending 的同名工具，也可手动执行：
.\trace.exe tool RUN_ID CALL_ID --registered TOOL_ID
```

`TRACE_LOCAL_TOOLS_CONFIG` 是 JSON 数组文件，`TRACE_LOCAL_TOOLS` 可直接提供同内容 JSON，两者择一。示例 [examples/local-tools.json](examples/local-tools.json) 含两个可运行工具：`search_text` 直接调用 rg 输出 JSON Lines；`file_metadata` 通过 [examples/file_metadata.py](examples/file_metadata.py) 读取文件并返回字节数和 SHA-256。

```json
[
  {
    "name": "search_text",
    "description": "Search local files using ripgrep; exit 1 means no matches.",
    "command": "rg",
    "args": ["--json", "--", "{{pattern}}", "{{path}}"],
    "input_schema": {
      "type": "object",
      "properties": {"pattern": {"type": "string"}, "path": {"type": "string"}},
      "required": ["pattern", "path"]
    }
  }
]
```

- `name` 生成稳定模型 ID `local_<hash>`；`trace tools` 输出 `local_name` 与 `adapter=local`。同名重复登记报错，既有注册表保持不变。修改命令路径不改变身份；操作者应保持名称的能力含义。
- `command`/`args` 以原生 argv 启动，没有隐式 shell。`{{name}}` 将一个字符串、数字或布尔值传成一个参数，`{{name...}}` 展开标量数组；`{{arguments}}` 显式传整份 JSON。替换只匹配整个 token，不拼接、不重新分词。使用 `--` 或合适的固定选项分隔数据与选项，由具体命令自身处理参数语义。
- `mode` 默认 `argv`；`stdio` 除了固定/映射 argv，还把原始参数对象加换行写入 stdin 后关闭。它是每次调用一个进程的 JSONL 输入约定，不是持久 JSON-RPC/MCP session。
- `input_schema` 是发送模型的能力声明，默认 `{"type":"object"}`。Runtime 检查参数 JSON 对象和 argv 映射所需类型，不实现完整 JSON Schema 校验；业务参数仍由工具本身校验。
- `dir` 设置进程 cwd；`env` 合并进程环境；`timeout_seconds` 大于 0 时设置该工具的最大运行时间，默认 0 仅受总预算/运行取消控制。相对 cwd/命令参数路径按启动 Trace 的环境解释；跨目录部署请在自己的配置中使用绝对路径。
- Action 保存稳定 ID、local name、原参数、实际 argv 和结构化 Worker 结果；stdout/stderr/exit code → tool-role 消息 → Evidence。正常退出只证明执行观测，不自动证明业务成功。工具输出保持原文，包括 JSON；不根据输出文本猜测产品状态。rg 的退出码 1 保持 1，不重新解释成进程成功。
- 超时、取消或崩溃的未知副作用保留 pending/uncertain，避免重放；超时暂停，显式 cancel 保持取消。核对实际结果后按既有手工回填流程处理。命令不存在、非零正常退出记录明确错误，不伪装成成功。
- manifest 的环境配置不进入模型 schema 或运行快照；原调用参数、argv、stdout/stderr 按审计链原样持久。需保密的凭据应由工具从环境或自己的凭据存储读取，避免把凭据放进模型参数、argv 或输出。

云端集成暂缓；后续优先通用 SDK/API 或现有 MCP 入口，避免逐家安装 CLI。

## MCP 与 Worker

设置 `TRACE_MCP_CONFIG` 为 JSON 文件路径，或设置 `TRACE_MCP_SERVERS` 为同内容的 JSON 数组；两者择一。配置由操作者保存，不复制到 run snapshot；HTTP headers 与进程环境不进入模型工具定义。示例文件内容：

```json
[
  {"name":"local-worker","command":"python","args":["C:/tools/mcp_server.py"],"dir":"C:/tools"},
  {"name":"service","url":"http://127.0.0.1:8000/mcp"}
]
```

```powershell
$env:TRACE_MCP_CONFIG = "C:/tools/trace-mcp.json"
.\trace.exe tools
.\trace.exe run RUN_ID
.\trace.exe tool RUN_ID CALL_ID --mcp TOOL_ID
```

- `tools` 进行真实握手和分页发现；结果中 `definition.function.name` 是稳定 ID，`remote_name`/`server_name` 保留执行身份。ID 由 server 名称和工具原名生成，跨 CLI 进程不变；配置更换地址仍沿用同名身份，由操作者负责保持名称含义。
- 服务声明相应capability时，额外桥接 `resources/list`、`resources/templates/list`、`resources/read`、`prompts/list`、`prompts/get`；使用独立 `mcp_rpc_` 稳定ID与 `remote_method`，不与同名远端工具冲突。resource/prompt-only服务不调用 `tools/list`，未声明能力不生成接口。一个MCP仍只算一个服务。
- 资源和提示词保留完整原始JSON，作为工具数据进入原Action/Evidence；返回prompt的role或文本不自动成为Trace系统指令。列表cursor由调用者显式续页；sampling、elicitation和订阅仍未接通。
- `run/resume/message/tool` 注册配置工具后，模型按稳定 ID 自动调用。显式 `--mcp` 只执行当前 pending call 的同一稳定 ID 与原参数，避免重新输入参数改变既有动作。无配置时未知工具保持 pending。
- 正常返回经 Action → tool-role 消息 → Evidence 保存；`mcp_result` 保留完整结果对象，包括非文本 content 与 structuredContent，`mcp_is_error` 保留工具错误标记。isError/协议错误/中断不升级 verified；返回文本只是观测，仍不是独立领域成功证明。
- MCP 传输断开、错 ID 或取消导致结果未知时，Action 留存错误并标为 uncertain、保留 pending、暂停；重复 resume 不重发。核对副作用后先 resume 到 waiting_tool，再用普通 `tool RUN_ID CALL_ID RESULT` 回填。人工回填仍为 unverified。
- CLI 退出关闭 MCP sessions；关闭失败返回非零。HTTP DELETE 接受或 stdio 直接子进程退出不代表任意外部资源已清理。已测协议版本为 2025-06-18/2025-03-26，不宣称完整实现 2026-07-28。
- exec 与本地工具复用 `internal/worker.Run`，保存 PID/status/副作用未知标志。输出先流式落入临时文件，大输出归档 CAS，内存只保留有限预览。进程退出、中断与业务成功分开。
- Planner 以持久 DAG、frontier、priority、capacity 和 lease 调度独立 child Run；`worker_message/status/wait/cancel/recover` 提供过程交换与控制。child 使用独立 MCP session；父任务等待清理结果，普通分支不消费父 worker 信箱。恢复不会自动重放未知动作。当前没有远程 worker 健康集群、容器隔离或后台常驻 dispatcher。
- Worker 容量按根会话整树计算：所有嵌套 Worker 和未结算 claim 合计默认 5，独立会话及普通 Branch 各自计数。根计划可显式设 1–32；子计划不扩大根上限。Claim、注册、恢复在原 Store 事务中复核；uncertain 或未完成清理保留名额。当前 visit 已结算、清理完成且任务经 `task_dispose` 明确处置后可释放名额，原 uncertain 历史保留。同一 Worker 的合法续作复用自身名额，满额时可用自己的当前身份完成一个本地子计划任务，无需再派生 Worker。工作区异步显示整树使用量，本计划计数单独标注。
- `worker_resume({"worker_id":"…"})` 继续失败/暂停的原 Worker/Run，保留完成动作，不创建新 attempt；cleanup 未完成、父任务停止或容量/依赖不满足时拒绝启动。等待输入先用 `worker_message` 补充，未决工具/未知副作用仍保留人工处理边界。`worker_recover` 核对持久清理/结算receipt并分类过期记录，不重启执行；`task_retry` 仅用于零动作且清理完成的失败尝试。活跃执行锁与内部同所有者续租共同防止 Store 等待造成误判，公共过期 ClaimTask 规则不放宽；中断原因另行持久保存。
- Worker派发时持久保存任务限定来源清单，只自动交付任务声明的 `AssetIDs` 和直接完成依赖，携来源Run、Evidence hash、Artifact/CAS引用；以user/data进入实际请求，不进入system、不复制父消息/动作或制造child证据。每次请求重新核验血缘与Evidence/CAS，缺失/改变/失效显式标记；恢复不重复写历史消息，旧请求保持原样。两种上下文压缩及工具选择预算计入清单，固定上下文过大时暂停。该能力不是工具权限沙箱。
- MCP progress、Provider delta 和工具输出分段写入同一事件流，WebUI 通过 SSE 增量读取。关闭先取消并等待 worker，再关闭父 MCP；非零退出与清理失败保留错误，不按成功清理处理。

## GitHub MCP / Skill 接入与更新

左侧 MCP 管理与 Skill 管理均提供“从 GitHub 导入”。支持仓库链接、`tree/分支/子目录`，Skill 还支持 `blob/分支/目录/SKILL.md`；分支名称可包含 `/`。需在 PATH 中安装 Git。导入记录与 checkout 保存在服务配置旁的 `integrations` 子目录，重启后保留。

- Skill 按仓库内的 `SKILL.md` 名称与描述发现，正文仍通过 `read_skill` 按需读取；同名导入冲突会报错并保留已有技能。
- MCP 优先读取 `.mcp.json`、`mcp.json` 或 `trace-mcp.json` 的 `mcpServers` 对象 / `mcp` 数组。无配置的仓库需填写其实际启动命令与参数；`${REPO_DIR}` 可引用导入目录。HTTP MCP 可通过普通 MCP 添加表单或 API 的 `mcp.url` 接入。
- 仓库依赖、构建步骤按项目说明安装。可取消“导入后启用”，先下载保存，再在显示的本地目录安装依赖并启用服务。不会把普通 GitHub 项目自动转换成 MCP。
- 有 GitHub 来源时同时显示 GitHub 链接和本地路径；Skill 额外显示实际 `SKILL.md` 路径。纯本地项只显示路径。已有本地 Git 项目可从 GitHub origin 推断来源；也可显式填写 `github_url` / `local_path`。
- “检查更新”拉取所选分支并比较提交，“立即更新”准备新 checkout、重建同一个 Agent，再保存和切换配置。失败保留旧 Agent 和配置；成功保留旧目录，API 返回 `previous_path`。本地有修改或历史分叉时保留原版本；tag / 完整 commit 链接固定版本，不自动追踪分支。
- 新 checkout 不复制旧目录中 Git 忽略的依赖或构建产物。需要这些文件的 MCP，先禁用服务、立即更新，在新路径安装/构建，再启用；启用状态下若新版本启动失败，旧服务继续保留。依赖变化不会自动执行安装脚本。
- 配置导入和更新需先暂停主任务并等待 Worker 结束。Git 操作与 MCP 重建可取消，操作期间配置/目录读取保持响应；更新完成后新请求使用新目录。

API 与界面共用实现：

```text
GET  /api/integrations
POST /api/integrations/import  {"kind":"skill","url":"https://github.com/owner/repo/tree/main/skills/example"}
POST /api/integrations/import  {"kind":"mcp","url":"https://github.com/owner/repo","disabled":true,"mcp":{"name":"example","command":"node","args":["server.js"]}}
POST /api/integrations/check   {"id":"导入记录 ID"}
POST /api/integrations/update  {"id":"导入记录 ID"}
```

已有 Git 来源项也可用 `{"kind":"mcp|skill|local","name":"实际名称"}` 检查或更新。本次真实验收导入 pi 的 Skill 文件及官方 Playwright MCP 源码，发现 25 个 MCP 工具，并通过 Agent 实际调用 `browser_close` 保存 Action/Evidence。测试用 checkout 已清理；这证明接入链路，不代表默认安装全部 GitHub 项目、外部 LLM 或全部 Playwright 功能。

## Provider 前提

需提供可用的 OpenAI-compatible Chat Completions endpoint、模型 ID，及该服务所需 API key：

```powershell
$env:TRACE_PROVIDER_URL = "https://YOUR_PROVIDER/v1"
$env:TRACE_MODEL = "YOUR_MODEL"
$env:TRACE_PROVIDER_KEY = "YOUR_TOKEN"
```

URL 可直接以 `/chat/completions` 结尾，否则会追加该路径。需要支持原生 function tool_calls；请求启用流式输出并聚合文本、thinking、tool-call 参数片段与 usage，兼容服务返回普通 JSON。截断/取消保留已收到片段，但不执行不完整工具参数。本地 HTTP fixture 覆盖 wire 协议，不等同真实外部服务验收。密钥不进入模型请求记录或运行快照；Web 服务配置将密钥保存在本地配置文件，读取界面时脱敏。

### 角色默认模型与 Run 绑定

服务配置的 `provider` 对应保留名称 `default`；`providers` 可增加具名连接，名称不得为 `default`。`role_models.orchestrator` 用于新建主 Run，`role_models.worker` 用于新派发的 Worker；未指定的编排者/Worker使用active_provider选择的默认连接（空或default沿用原provider）；显式角色配置优先，信息收集未指定时继承Worker。Web“模型与 API”页左侧列出已保存配置，右侧编辑所选连接；点击“新增配置”添加地址、模型和API Key。“保存配置”不切换默认，“保存并设为默认”在一次提交中保存并切换，也可从左侧对已保存配置单独“设为默认”。有未保存草稿时先保存或丢弃。密钥占位[stored secret]保留旧值，留空清除；GET不返回真实密钥，保存文件权限0600。只改模型配置无需暂停或重连MCP；已开始的运行继续原路由，更新其密钥后可主动暂停再恢复。恢复仍用该Run原Provider/模型，切换默认不会重绑旧Run；改端点或撤销能力仍按原身份门报错。检测面板对应当前编辑的default或命名连接，与当前默认配置的顶部状态分别归属；编辑、切换或配置版本变化会撤销陈旧结果。

以下为合并到现有服务配置的最小无密钥示例（地址和模型名需替换为实际值）：

```json
{
  "provider": {"url": "http://127.0.0.1:8000/v1", "model": "main-model"},
  "providers": {
    "worker-api": {
      "url": "http://127.0.0.1:8001/v1",
      "model": "worker-model",
      "model_capabilities": {
        "worker-model": {"image_input": false, "reasoning_efforts": ["low", "medium"]}
      }
    }
  },
  "role_models": {
    "orchestrator": {"provider": "default", "model": "main-model"},
    "worker": {"provider": "worker-api", "model": "worker-model", "reasoning_effort": "low"}
  }
}
```

`model_capabilities` 按实际模型 ID 声明视觉能力及允许的推理强度，不是连通性检测结果；旧的 Provider 级 `image_input/reasoning_efforts` 仅补充其当前默认模型未显式声明的能力。更换默认模型时，应保留仍需恢复的旧模型能力项，避免借用新模型能力。API 可在 `POST /api/runs` 请求中以 `model_selection` 覆盖本次主 Run 的角色默认，例如 `{"goal":"目标描述","model_selection":{"provider":"worker-api","model":"worker-model","reasoning_effort":"medium"}}`。

新 Run 持久保存实际 provider 名称、端点身份、模型、推理强度和视觉能力；分支保留源绑定，恢复不因角色默认改变而切换模型。连接缺失、端点身份变化或保存的能力不再满足时明确报错，不自动 fallback；密钥仍只从服务配置读取。没有绑定的旧 Run 保留旧行为。请求账本记录实际模型/推理强度，成本按实际请求的 Provider/model 计价，并计入父子整树预算，不按角色标签估价。

### 阶段模型、根合同与执行边界（通用Agent）

- 在服务配置 `role_models.stage_routes` 声明精确允许列表，例如 `[{"provider":"worker-api","model":"worker-model"}]`。PlanTask可选 `model_selection` 和 `required_image_input`；未指定沿worker默认。允许路由及各自视觉/effort能力进入模型上下文，不暴露密钥/URL。空列表禁用显式阶段选模；换route不借旧effort。首次claim持久冻结实际 `model_binding`，该字段为服务端所有；started任务和原Run不原地换模，零动作retry/恢复也不随新默认漂移。
- 需要换模时先结算旧阶段并保存真实checkpoint，新task保留业务目标、输入/资源/验收/对象版本，显式 `handoff_task_ids`，对未完成旧task使用合法superseded并重接pending依赖；旧ID不成为成功依赖，uncertain不重放。
- `POST /api/runs`可带 `root_contract:{"version":1,"requirement_ids":["report"]}`，引用同请求的requirements。有限资产集合应先创建Run及资产，再在执行前 `POST /api/runs/{id}/root-contract` 传 `asset_ids`；合同hash服务端生成，之后不可替换或弱化。只有显式合同Run才允许 `stage:true` 的已结算失败阶段不直接否定根目标，根要求仍须实际通过。开放研究不强制穷尽未知空间。
- 可选 `evidence_scope:{"object_id":"report-source","version":"sha256:…"}` 固定任务对象/版本，同一声明作用域续访可沿用通过来源验证的证据；不同版本拒绝。它不是对外部版本未变化的自动观测，发生实际变化需新阶段/重新取证。`expected_artifacts`仅提示，硬验收使用requirements和实际文件/页面/执行证据。
- 新Run的 `exec`持久默认 `timeout_seconds:300`，创建API可配置1..86400；每次exec可显式覆盖同范围。这是单进程及I/O边界，不是子Agent求解预算；旧Run缺字段保留历史边界。超时保partial/CAS和uncertain、不自动重放。Unix结束后核对并终止原进程组活成员，清理失败或缺持久进程结果不释放Worker资源；脱离session子孙与Windows一般树清理不据Linux结果宣称已证明。
- `worker_message`返回单条 `receipt/created/status=accepted/processed=false`，接受不等于投递或实际处理。例行状态只显示有界活动，`worker_history`显式分页完整messages/workers/tasks，`open_help:true`读当前未解求助，已注册child加 `parent:true`读共享父账本；页是当前快照顺序，并发resolve可移动offset，不是持久游标。

### 连通性检测

模型与 API 页提供 **检测连通性**：使用表单当前地址、模型及密钥发送一次短 Chat Completions 请求（最多64输出tokens），不保存草稿、不创建Run，也不替换当前Agent。已保存密钥占位只引用所选配置在服务中的对应密钥；保存配置、读取和启动服务不会自动检测。支持取消，编辑连接字段、导航或重新载入配置会撤销旧结果；错误仅显示固定类别，不回显上游正文或凭据。

完整文本响应为检测成功；仅推理或达到输出上限显示“已响应但未形成完整答案”；Mock不发网络。连通性成功不代表流式、工具调用、视觉能力或研究任务成功。A3已在隔离Ubuntu服务器通过全量Go/vet、Node和真实桌面浏览器fixture验收，线上外部Provider仍需实测。

### 信息收集与搜索源

任务创建时先固定根目标，再整理已有附件、资产、来源引用、对象版本、工具与待核缺口；后续按变化更新。简单任务直接使用已有输入，缺口不成为强制扫网或运行准入条件。独立收集任务用 `execution_role=recon`，占用同一整树 Worker 名额；后续任务只接收选定的来源清单，不复制整段父运行历史。

“模型与 API”页可单独设置收集模型的 Provider、模型和推理强度，也可继承 Worker 默认。首次启动后固定实际绑定，恢复不随默认配置漂移。只有需要独立收集任务时才使用该角色，不给每个任务强插一次模型调用。

同页支持 FOFA、Quake、Shodan 的启用、凭据、并发、间隔、查询最大页码，以及根树累计请求次数和请求结果条数预留额度。保存后“检查已保存配置”只读取账号接口，不搜索、不创建运行，未保存草稿不参与检查。服务端保存密钥，GET 仅返回占位。模型通过 `search_sources` 查看可用来源，通过 `source_search` 查询和分页；累计额度在真正的 Worker 整树共享，普通 Branch 独立。结果保留来源及 partial/错误/未知状态，候选资产不自动证明当前目标状态或漏洞。

## 上下文、会话与 Skills

默认上下文窗口 250000、输出预留 16384；`TRACE_CONTEXT_TOKENS`、`TRACE_MODEL_CONTEXT_TOKENS`、`TRACE_OUTPUT_RESERVE` 可覆盖，实际输入预算取窗口与模型上限的较小值再扣除输出预留。CLI 未显式设置时保留运行已有配置。

默认 `TRACE_COMPACTION_MODE=semantic`：通过现有 Provider 总结历史，原始消息、工具配对、Evidence 和 CAS 保留，摘要请求与实际 token 消耗进入原有预算。摘要保存源消息 ID/hash 与请求记录，恢复时复用有效摘要；`projection` 明确选择确定性摘录。估算使用保守 UTF-8 wire bytes，并非模型 tokenizer。system/tools、最新完整轮次或单个历史工具批次本身超限时暂停，摘要失败也保留原始记录。

P2从最终messages/tools派生manifest及遗漏清单，明确图片hydration前/后边界，不冒称包括HTTP额外字段；`context_ledger.go`只从成功完成的Action/CAS派生文件与产物索引，摘要自述与failed edit不算修改。Node使用revision/prefix/message_head/schema增量可重建缓存，Go仍唯一真值；动态工具、压缩和late input重新对齐canonical上下文，不建立第二持久账本。

WebUI 的 Steer/Follow-up 持久入队，在工具批次/回复边界交付；末轮同时到达的两类消息在下一请求一起可见。`Message` 用于空闲运行继续对话；暂停后先 Resume 或入队指令。分支从完整消息前缀建立独立 Run，保留父消息来源和工具选择，不修改父运行。此实现不是完整会话树切叶系统。

Skill 来源优先级：`TRACE_SKILL_PATHS` 中显式路径（前者优先）→ 当前项目 `.trace/skills` → 用户目录 `.trace/skills`。Windows 环境变量用分号分隔，Unix 用冒号。只发现 `SKILL.md` 的 name/description/path/source，模型通过 `skills_list`、`read_skill` 按需读取正文；同名按优先级选择。当前是基础 frontmatter，不是完整 YAML/插件系统。

`read_skill` 只接受不超过 **64 KiB（65536 字节）** 的有效 UTF-8 普通文件；超限或读取期间检测到文件变化即报错，不截断后当作完整 Skill。返回正文、path/source、字节数、SHA-256 和不可变 CAS artifact；SHA-256 标识读取到的**完整原始文件字节**，不是 JSON 包装或 stdout 的哈希，Action 保留对应 raw CAS 引用。后续读取仍读取当前文件；需要固定版本时传 `expected_sha256`（先前返回的 64 位小写十六进制哈希），不匹配即报错，不自动改读旧 CAS。

`disabled_skills` 按技能名禁用：管理端仍可查看该项与正文，模型目录排除它，模型 `read_skill` 也明确拒绝；不会转而启用低优先级的同名 Skill。停用不撤回已有请求中的历史正文或已归档 CAS。

## 按需工具 schema

`tools`/`mcp` 始终保留完整目录。完整 schemas 加固定提示词超出输入预算时，模型先获得内置工具，再使用：

创建Run可显式指定 `tool_schema:{"mode":"deferred"}` 从核心工具开始，或 `tool_schema:{"mode":"narrow","ids":["实际稳定ID"]}` 从明确子集开始；默认auto保持原行为。当前选择在child/branch/restart保留，完整目录始终可达。ModelRequest.tool_metrics记录schema数量/序列化字节/保守估算和累计search/select动作数，不是Provider计费token、缓存命中或TTFT。窄启动可能增加发现轮次，不自动猜相关工具。

```json
{"name":"tool_search","arguments":{"query":"frida","offset":0,"limit":10}}
{"name":"tool_select","arguments":{"ids":["实际搜索返回的稳定ID"],"replace":false}}
```

搜索按工具名、描述和来源匹配；空 query 可分页遍历全部外部工具，默认每页 10、最多 50。选择会写入 `Run.tool_selection.ids`，完整 schema 出现在下一模型请求，恢复/分支/child 保留；`replace:true` 替换，空 IDs 配合 replace 清空。内置工具和 exec 始终可见；非法 ID 或超预算选择不改快照。WebUI 运行工具选择使用同一 `PUT /api/runs/{id}/tools` 接口，不另建执行目录。

## 文件与 CAS

内置 `read` 支持 byte range 或 `start_line/line_count`，流式计算全文 hash、返回有界分页、字节保真 base64、MIME；PNG/JPEG/GIF 可返回图像尺寸。`write/edit` 使用原子替换，支持 expected_hash、多项精确编辑和 diff；编辑仍需载入旧文件。图片字节与尺寸读取不代表已向模型发送原生视觉内容。

超过 16 KiB 的 stdout/stderr/工具结果以 SHA-256 CAS 引用保存在 Snapshot，原始字节可通过 `artifact_read` 和 Web 下载读取。分页使用 `next_offset` 与 base64 保持二进制/UTF-8 字节边界，默认 artifact 页 4 KiB、最大 64 KiB。下载及证据验证重新校验 hash；页面预览复用未变化文件的校验结果。流式 worker 输出有磁盘暂存与有限内存预览；MCP/Provider JSON 解码及部分文件读取仍可能占用完整响应内存。

原生搜索的 `search_response` 原始回执可能包含账号信息或密钥回显，在原 CAS 中保持原字节并加持久审计专用标记。Web 审计读取保留；模型通过归一化 items/status 使用搜索结果，`artifact_read` 和 `read` 在分页前按完整 hash 拒绝这些原始回执（含复制路径），`edit` 在生成旧字节 diff 前同样检查。该约束作用于内置文件工具，不是任意 shell 或本机用户的操作系统隔离。

P2 builtin写/改按解析后的真实路径互斥，保留expected_hash与原子替换，非任意exec文件写入锁。CAS增加UTF-8安全head/tail、offset/行数预览；完整证据验证仍读取完整CAS，不把预览当执行正文。

## 本地 WebUI

`trace.exe serve --addr 127.0.0.1:8765` 后打开 [http://127.0.0.1:8765](http://127.0.0.1:8765)。配置默认 `TRACE_DATA_DIR/service/web-config.json`，可用 `--config FILE`。纸/白/黑主题；左侧直达模型与API、MCP、Skill；任务/档案、暂停/取消/恢复、消息/分支、Planner/Worker、图/证据/报告/产物共用Agent/Store和持久SSE游标。

MCP先看服务简介/连接/启停，再进入工具目录及独立运行选择；支持 `description/disabled`，默认启用，禁用保留配置且不启动/注册。目录发现不证明依赖引擎可用。配置更新先建候选Agent，失败保留旧配置，主Run/Worker活动时不替换。

配置放 `service` 或Run目录外；B1普通根JSON误识别已修复，损坏Run仍报错。CLI/Web使用相同 `TRACE_DATA_DIR` 与配置文件即可共用完整配置；相对路径仍按cwd解释，跨入口建议绝对路径。创建时预算/成功条件原子保存，首次执行后成功合同锁定。名称/归档仅改元数据；正文检索和消息/调用/事件定位保留真实身份。消息operation_id支持丢ACK核对/重试与待投递队列编辑，浏览器只保存操作ID。图回放只读，缺失历史正文不从当前状态补造；关联CAS可预览图片/音视频/PDF，不支持的格式仍可下载。资产覆盖仅以显式included资产为分母，observed/verified/negative/uncertain与completed分开。Go端单用户，公网Basic由部署层提供；范围见 [开发计划.md](开发计划.md)，验收见 [DEPLOYMENT.md](DEPLOYMENT.md)。

## 预算与用量

- `TRACE_MAX_ROUNDS`：累计模型请求上限，默认 0（不限；当前 `DefaultBudget()` 为零值）。
- `TRACE_MAX_ACTIONS`：累计真实执行动作上限，默认 0（不限）。
- `TRACE_MAX_SECONDS`：累计活跃墙钟秒数，默认 0（不限）。
- 显式 0 表示不限；未设置则保留既有运行的持久上限。运行耗尽后用提高后的环境变量再 `resume`。
- 模型请求、响应、工具动作数与已知 prompt/completion/total tokens 持久保存。缺失字段保持 null，并记录缺失用量响应数量。
- 中断执行时间持久记录；崩溃遗留活跃区间在恢复时保守计入预算，因此可能包括停机时间。
- 父子费用按真实Worker后代及request身份去重；Provider设置中的`costs`采用十进制字符串价格，必须填写币种、来源、生效时间。价格在请求时固定，旧请求无价格、未返回请求、缺失usage均保留未知；reported token乘价格仅为估算，非供应商账单。费用池是参考额度，不触发执行硬限。
- P2显式`TreeBudget`通过原Store事务限制整树深度、累计Worker/请求/token/活跃run-seconds/费用，零值不限；未知usage/价格不当0，在途请求预留是估算而非账单保证，不复制父限额到每个child。显式时间预算进入checkpoint窗口后停新派发并要求yield，硬deadline不延长，恢复沿用原child。

## 结果与证据

“报告与文件”默认展示阶段、发现、最终总结及实际登记文件；stdout/stderr/搜索/目录回执在内部工具记录中，不作为报告交付。总结按来源 digest、正文 digest 和版本存为实际 CAS Markdown 文件，提供中文、English 两份正文下载。默认下载最终总结，显式 `report_id` 下载固定历史版本；来源失效会生成当前状态的新版本，旧字节保持。

实质执行完成但缺译时，沿同一 pi 对话补齐报告，不重复工具，也不以译文替代业务或清理证明。补译回合不提供工具，返回的工具调用也被服务端拦下；补译失败暂停报告交付，原业务结果继续核验。补译期间新用户/Worker 输入保持待投递，报告交付后继续正常业务回合。普通聊天不强制补译；正文完整性检查不替代人工翻译质量或真实任务质量评估。

`run.status=completed` 与 `result.status=success` 分开。未设置验收要求时，普通文本仍为 incomplete；已设置的验收要求、任务谓词、Planner及Worker约束未满足时，模型结束回复会触发继续执行。显式 incomplete 或无效结构化结果按完成合同继续执行，而非直接按普通回合结束；普通无验收聊天不强制无限循环。缺少具体用户输入可调用 `request_input`，持久保存 `paused` 和 `waiting_input: …` 原因；补充输入后再恢复，原验收要求保留。

P2第18项以动作/证据/计划/用户输入的语义指纹去重completion audit；相同输出、流、心跳及audit本身不算新进展。相同缺口保持running，至多记录一次stalled/replan_requested并提示更换具体动作或重规划；**没有进展不隐式pause/停止，也不加轮数上限**。真实新进展重新允许具体audit；显式预算、操作暂停、request_input及原失败处置仍各自生效。

模型可提交下列结构化最终内容（证据 ID 必须来自真实工具返回）：

```json
{"trace_result":{"status":"success","summary":"实际发现","evidence_ids":["ev-7"],"answers":[{"kind":"answer","value":"observed-marker","evidence_ids":["ev-7"]}],"reproduction_steps":["实际复现步骤"],"cleanup_status":"unknown"}}
```

账本核对事件序号、call/request 关联、SHA-256、tool 消息和已完成动作；只有正常退出的执行证据可验证，CAS 原始内容参与校验。答案还需逐字出现在引用 stdout 中。缺失/损坏/手工证据及虚构答案保留 incomplete，模型自述 verified 无效。任务自身成功谓词、依赖与 worker 清理状态约束父结果；通用领域资源清理仍不能仅凭模型自述升级 verified。

也可在 create 前设置 `TRACE_SUCCESS_CONTAINS`：仅当真实、正常退出工具 stdout 包含该固定串时视为谓词成功。它只验证字符串条件，不证明复杂漏洞语义；模型可自行构造 stdout，故它不是对抗模型伪造内容的独立业务裁决器。结构化成功同样证明来源和引用一致性，不替代域专用验证。

实际模型请求、`verification_status.success_predicate` 与 completion audit 显示该谓词的精确值、匹配域及证据/未通过原因。它独立于 requirements，不是 `evidence_check` 的 requirement ID；无需为它虚构或追加命名 requirement。

首次执行前可通过 `PUT /api/runs/{id}/requirements` 固定验收要求；`evidence_check` 支持 `contains`、`equals`、`regex`、`json_equals`，并可用 `require_review` 要求独立模型请求核对精确输出。全部声明要求通过后，普通完成回复也可产生 success。审阅按配置模型运行并单独存档请求/响应，计入原Run预算；它是模型判断，不是独立复现。completed Run可审阅，paused Run需先恢复才能启动模型审阅；纯本地谓词检查不启动模型。分支保留要求，但不继承父Run的检查结果。

`finding_submit` 保存严重性、根因、影响、修复建议、步骤及真实观测引用；`finding_review` 只产生审阅状态。`finding_retest` 必须关联在该发现最新修订后启动的新执行，匹配原工具、参数、Adapter、Command及已记录MCP/local路由。元数据回显、原证据和仅有复测建议均不计复测；通过只证明相同探测再次满足声明谓词。`triage=fixed` 是处置标签，不替代新执行结果。

`cleanup` 要求预声明探测工具和参数SHA-256；已知修改动作或Worker仍活动/结果不确定时保持 unknown，动作结果和Worker结束之后必须重新启动探测。它不监控未登记其他Run或外部进程的副作用，也不证明工具背后的目标、脚本、配置从未变化。

升级后的主题/要求哈希使用排序JSON对象键与UseNumber。旧版hash不匹配时，原check/review/retest会失效为unknown或submitted；按当前证据重新检查/审阅，复测证据须满足当前执行身份与发现修订后的时序约束。历史请求响应和原始Evidence/CAS仍保留，不自动重签旧判断。来源交叉校验也不等于整套Store遭到任意重写时的防篡改保证。

## 已知边界

当前已有本地 JSON/OS 锁、CAS、Skills、语义摘要、Provider/工具流式事件、持久 Planner/child worker 和本地 WebUI。SQLite为显式离线迁移候选后端，默认JSON保持；尚无远程 worker 集群/容器隔离、模型 tokenizer、费用/token 金额上限、完整 extension/session tree。CAS原生输入附件已接通，模型配置显式开启 image_input 才发送图像块；真实外部视觉模型尚未验收。Windows 历史已运行进程级、独立二进制与独立 Frida 验证；本轮全部产品构建/运行验收只在Ubuntu服务器，历史已通过真实DeepSeek长任务/独立证据审阅与新执行复测、REA单元/集成及Chromium/Electron活体；30分钟soak使用受控Provider，不是30分钟真实DeepSeek。各批结果不替代未发布改动验收。服务器访问与运维见 [DEPLOYMENT.md](DEPLOYMENT.md)。

正式源码回归通过 `go test ./...`、`go vet ./...`、`node internal/web/render_test.mjs` 和 `node internal/web/integration_interactions_test.mjs` 运行；临时工作目录收尾删除；本轮原始验收日志、历史运行证据快照及已核对截图归入发行版 validation，不导入正式用户 state。当前范围与refs来源见 [开发计划.md](开发计划.md)。
