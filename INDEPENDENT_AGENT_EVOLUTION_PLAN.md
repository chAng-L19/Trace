# codex-redteam-agent 第一性原理演进计划

> 权威路线文档。Phase 0–5 的已验收行为以 `docs/acceptance/phase-0.md` 至
> `docs/acceptance/phase-5.md` 为准；旧版架构草案不再作为验收依据。

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

## 修订后的后续阶段

### Phase 6：Thin Tactical Loop、工具可见性与反误判语义

Phase 6 不建设由 Runtime 主导的复杂 Planner。生命周期仅作为质量 Gate；模型在每个 Gate 内自由生成和排序战术动作。

#### 6A：Model-led Tactical Loop

- 当前生命周期 Gate 只约束所需证据和清理义务，不预先规定工具动作。
- 模型可以创建、暂停、否定、重开和合并 Intent/Hypothesis。
- 每次工具调用都记录为 `ExplorationRecord`，不自动晋升为 Fact 或 Evidence。
- 引入 `ObservedMiss`、`CoverageClaim`、`HypothesisState`、`VerifiedNegative` 四种不同语义。
- 未完成探索不得关闭整个攻击方向；重开条件必须持久化。
- 重复动作只产生诊断信号，不由 Runtime 擅自终止模型搜索。

#### 6B：AI-Friendly Tool Projection

模型上下文使用有界投影，保留完整原始 Artifact 引用。HTTP/API 投影至少包含状态码、Header 差异、Body 长度/差异、时间、请求/响应 hash、枚举覆盖和原始 Artifact 引用。

#### 6C：Context Continuity 与 ReconDigest

默认保持一个主模型上下文，仅在上下文退化边界切段。`ReconDigest` 必须包含目标状态、原始 Artifact 引用、已尝试动作、确认观察、未验证假设、矛盾和重开条件；Digest 是导航投影，不是真实来源。

#### Phase 6 验收

- 不依赖预置漏洞名称即可由模型生成新搜索节点。
- `ObservedMiss`、不完整枚举和工具失败均不能自动生成全局 `VerifiedNegative`。
- 新能力或新证据可以重新激活旧分支，恢复后分支状态和顺序一致。
- Runtime 不覆盖模型的战术优先级；只执行作用域、预算、凭据、幂等和证据 Gate。
- 工具结果同时具备有界差异投影和完整 CAS 内容，完整 Artifact 回读成功率 100%。
- 在相同模型、目标和预算下，长运行输入 Token 较 Phase 5 投影基线下降，攻击路径完成率不下降。
- 固定动作 DAG 不再作为模型搜索空间；`generic-adaptive` 仅作为旧调用方兼容工作流。

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
