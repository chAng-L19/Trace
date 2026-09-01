# codex-redteam-agent 第一性原理演进计划

> 权威路线文档。Phase 0–6 的已验收行为以 `docs/acceptance/phase-0.md` 至
> `docs/acceptance/phase-6.md` 为准；旧版架构草案不再作为验收依据。

## 定位与第一性原理

项目根目录：`E:\\cli\\codex-redteam-agent`

目标是构建证据驱动、可持久恢复、最大化强模型战术能力的专业红队 Agent 平台。

- **模型负责战术**：目标理解、假设生成、工具选择、局部搜索、代码编写、动态分支和重规划。
- **Runtime 负责不变量**：作用域、凭据、预算、租约、幂等、CAS、证据 lineage、清理和终态裁决。
- **SearchGraph 是战术账本**：记录模型提出的假设、尝试、未验证线索、覆盖边界和重开条件；它不是事实库，也不负责替模型排序。
- **EvidenceGraph 是权威证据链**：append-only，任何晋升都必须经过来源、目标、父证据、工具和完整 Artifact 校验。
- **AssetAttackGraph 是已验证关系图**：只接受已验证资产、身份、关系、Finding 和攻击路径。
- 默认先使用一个最强主模型和连续上下文；多 Agent、复杂调度和分布式能力必须通过等预算实测证明收益。

## 已完成阶段

### Phase 0：权威基线

冻结独立 Git 基线、Python 3.12、SQLite/MCP/Goal/Evidence/Terminal 快照和确定性 fixtures。

### Phase 1：Core Contracts 与 Ports

建立标准库 Core、版本化领域类型和 Provider/Tool/Worker/Store/Event 端口，保留旧 Runtime 适配层。

### Phase 2：AgentService 与持久生命周期

统一 `start/run/submit_observation/status/cancel/events`，保留 CAS、Lease/Fencing、恢复、取消和多目标隔离。

### Phase 3：Provider-Agnostic ModelLoop

Runtime 驱动模型循环，记录 Prompt/Response hash、Provider、Capabilities、Usage，支持结构化输出、工具调用、并行、流式和恢复。

### Phase 4：Conversation、Context 与 Budget

持久化完整 transcript，建立可追溯压缩、受保护目标状态和 action/token/time 三维预算。

### Phase 5：Worker Plane 与 Artifact Store

实现 Local/MCP/Codex/Docker Worker 边界、隔离 workspace、取消/reconcile、SHA-256 CAS、FTS5 和大输出有界投影。

### Phase 6：Thin Tactical Loop、工具可见性与反误判语义

已完成模型主导的薄战术循环、append-only ExplorationLedger、ObservedMiss/VerifiedNegative
语义分离、分支重开、ReconDigest、完整 Artifact 与有界模型投影。

## 修订后的后续阶段

### Phase 6.1：Stateful MCP Capability Plane（Phase 7 基础）

在不改变 Phase 7 Evidence 目标的前提下，先补齐有状态专业工具平面。该层参考
OpenCode 的 MCP 状态/目录刷新、`cc_src` 的 scoped config/取消/压缩边界、
Playwright MCP 的 accessibility snapshot 与 IDA Pro MCP 的显式 database 会话。

#### 6.1A：Run-scoped MCP lifecycle

- Playwright 浏览器上下文和 IDA supervisor client 默认按 run 隔离；共享 MCP 仍保留可选配置。
- `{run_id}`、`{workspace}`、`${ENV_VAR}` 在创建 client 时延迟绑定。
- MCP roots 指向 run workspace；超时、取消和终态关闭贯穿 ToolPort/WorkerPort。
- `tools/list_changed` 刷新工具目录；状态投影区分 disabled/failed/duplicate/catalogued/connected。
- IDA 只清理本 run 通过 `idb_open` 获得的 database session，不根据全局 `idb_list` 猜测所有权。

#### 6.1B：Token-efficient capability catalog

- Playwright 默认高价值目录优先 `browser_find`/`browser_snapshot`，省略安装等低频工具；可用 `include_tools=["*"]` 恢复全量能力。
- IDA 默认分析目录覆盖 database 管理、反编译、反汇编、xref、调用图、数据流和内存读取；修改类工具按需显式加入。
- 工具 schema 仍原样提供给强模型；额外的紧凑 server catalog 只用于观测和后续动态选择，不替代完整 schema。
- 工具结果继续遵守完整 CAS + 有界上下文投影，不把大输出塞入 OperationState。

#### 6.1C：Provider adapters

- Playwright preset 使用官方 `@playwright/mcp` stdio server、isolated/headless、图片响应省略、禁用 codegen 和 workspace output。
- IDA preset 使用 `idalib-mcp --stdio`，所有分析调用必须显式携带 `database`。
- `mcp-doctor` 输出 server status、discovery errors、schema hash、side-effect annotation 和有界工具目录。

#### Phase 6.1 验收

- 官方 Playwright MCP initialize/tools-list 成功，真实本地页面的 navigate/snapshot/find 完成并关闭 run client。
- 两个 run 的 MCP client、cwd、roots 和 browser/database state 不串扰。
- IDA 配置、工具筛选、显式 database、取消和按 run 清理协议测试通过；实机验收在具备 IDA Pro 8.3+ 与 idalib 的环境运行。
- 工具变更通知刷新目录，重复 process signature 不重复启动。
- 完整 Phase 0–6 快照、全量回归、wheel、隔离安装、自检和五个公开 MCP schema 均保持兼容。

### Phase 6.2：IDA Free 只读桥接

IDA Free 9.4 不提供 `idalib`，且 IDA Pro MCP 插件明确排除 Free。因此不伪装成
`idalib-mcp`，而是通过独立 stdio MCP bridge 启动 `ida64.exe`/`idat64.exe` 的
`-A -S` IDAPython 会话。桥接器按 database session 隔离进程，仅暴露函数、导入、
反编译、反汇编、xref、字符串、字节和整数读取；修改类 API 不进入默认目录。

#### Phase 6.2 验收

- 安装器路径被识别并拒绝，不会被误报为可用 IDA runtime。
- bridge 的 initialize/tools/list、显式 database 和 session close 协议测试通过。
- IDA Free 实际运行时完成 `idb_open → decompile/list_funcs → idb_close`，原始输出进入 CAS。
- IDA Free 不可用时，MCP status 保持 failed/pending，不生成 Evidence 或成功终态。

### Phase 7：Evidence、Finding 与专业终态

保留 append-only EvidenceGraph 和 TerminalJudge。负向控制只证明明确测试条件；“已耗尽”必须具有机器可验证覆盖、未解决矛盾检查和清理证明。

### Phase 8：Web/API Capability Pack

建设 HTTP、浏览器、API Schema、认证态、会话、入口发现、差异分析、受控验证和负向控制，并使用确定性易受攻击/已修复 fixtures 评估可见性、首触达延迟、攻击路径完成率和误证伪率。

### Phase 9：Codex/MCP Adapter 收敛

MCP、Codex、standalone 全部调用同一 `AgentService`；保持五个公开工具 `redteam_run/status/evidence/cancel/events`，兼容旧 OperationRuntime。

### Phase 10：CLI、TUI、API 与 Web Workbench

UI 仅投影 AgentService 状态；SearchGraph、EvidenceGraph、攻击路径、预算、事件流和 Artifact 查看均支持断线恢复。

### Phase 11：动态多 Agent 与分布式 Worker

仅在能力缺口、上下文隔离或并行搜索带来收益时创建 Specialist。共享原始 Evidence/Artifact 和不确定性，不共享未经验证的结论。相同 Token/时间预算下完成率提升不足 5% 或成本超过 2 倍时，默认保持单 Agent。

## 评测指标

- 攻击路径完成率、首触达延迟、每分钟有效动作数；
- 每动作/Token 暴露的新节点数；
- 误证伪率、分支重开召回率、重复动作率；
- 压缩后原始证据回读率、Digest 信息损失率；
- 相同模型/目标/预算下的输入 Token、Wall-clock 和终态准确率；
- 干净目标的错误成功率和清理完成率。

## 迁移规则

- 新 Core 类型和 Ports 长期稳定；Provider、MCP、Codex、Docker、CLI 都是 Adapter。
- SQLite 只做向前、可重复、非破坏性迁移；Phase 6 schema version 从 8 升至 9。
- Artifact 使用内容寻址；原始数据永远优先于摘要、图和 RAG 索引。
- 旧 `OperationRuntime` 和 `generic-adaptive` 保留兼容语义，新的模型战术循环通过 `AgentService` 增强，不破坏 Phase 0–5 快照。
