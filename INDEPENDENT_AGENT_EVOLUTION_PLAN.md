# codex-redteam-agent 第一性原理演进计划

> 当前权威计划已升级为“Lean Transparent Agent Refactor”。详细方案、Pi 参考映射、
> 复杂度预算、阶段验收和删除边界见 `docs/architecture/lean-transparent-refactor.md`。

## 定位

构建一个证据驱动、可持久恢复、最大化强模型战术能力的专业红队 Agent Harness。

```text
模型：理解目标、生成假设、选择工具、动态搜索、编写代码、重规划
Runtime：作用域、凭据、预算、幂等、CAS、Lease、Evidence lineage、清理、终态
```

轻量化不是减少红队能力，而是删除重复状态、重复映射和重复编排。任何安全不变量
和机器可验证证据都不得因降低 Token 或代码量而削弱。

## 参考依据

| 项目 | 版本/提交 | 吸收内容 |
|---|---|---|
| Pi coding-agent | `9767ba275f3e9a5ee0f5c5342249b629ab1b2282` | session tree、append-only journal、selected tools、OutputAccumulator、compaction boundary、resource loader、生命周期 hooks |
| OpenCode | `7774461bbf7bd0600070cdede4fe8b9d9f301bf4` | MCP 状态、工具目录、连接生命周期 |
| `cc_src` | 本地参考 | scoped config、取消、缓存失效、Token-aware 输出 |
| Playwright MCP | `7e0457a7cbf88823bf0146d12c46ae12c6818247` | accessibility snapshot、隔离 browser、read-only annotations |
| IDA Pro MCP | `3349ae30c6eb7fa1c14b158ff71bfc7c3081bb51` | explicit database、profile、supervisor/worker、cursor/cancel |

## 已完成基线

### Phase 0–5

已完成：基线冻结、Core Contracts、AgentService 生命周期、Provider-agnostic ModelLoop、
Transcript/Context/Budget、Worker Plane、CAS Artifact Store、Lease/Fencing、幂等和恢复。

### Phase 6

已完成：模型主导的薄战术循环、ExplorationLedger、ObservedMiss/VerifiedNegative、
分支重开、ReconDigest、有界工具投影和完整 Artifact。

### Phase 6.1

已完成：run-scoped Playwright/IDA Pro MCP、roots、工具目录刷新、取消、资源清理、
`mcp-doctor` 和 Token-efficient capability catalog。

### Phase 6.2

已完成可行性结论：IDA Free 安装器和 GUI 不作为项目内置后端；IDA Free 不提供满足
本项目结构化、可取消、可验证 Agent 集成所需的 API/插件能力。用户自行接入 IDA Pro
MCP；项目只维护通用 `ida` 配置与 adapter。

当前基线：

```text
HEAD: 99288e9 fix: remove unsupported ida free bridge
pytest: 280 passed, 1 skipped
compileall: passed
```

## 当前总计划

详细阶段定义见 `docs/architecture/lean-transparent-refactor.md`。

| 阶段 | 目标 | 状态 |
|---|---|---|
| L0 | 复杂度/调用图/行为冻结 | 已通过 |
| L1 | 唯一 AgentService 入口与边界收敛 | 待执行 |
| L2 | SessionJournal 透明会话树 | 待执行 |
| L3 | 单一 AgentLoop | 待执行 |
| L4 | ToolRegistry 与按需可见工具 | 待执行 |
| L5 | BoundedOutput 与流式 Artifact | 待执行 |
| L6 | ContextBudget 与可追溯 Compaction | 待执行 |
| L7 | ResourceResolver 与透明扩展 | 待执行 |
| L8 | EvidenceGate 收敛 | 待执行 |
| L9 | MCP/Worker 适配器瘦身 | 待执行 |
| L10 | 删除旧编排与发布门 | 待执行 |
| L11 | 透明度、Token 与能力评测 | 待执行 |

## 硬性目标

- 生产代码目标 ≤16,000 行、模块 ≤80 个、单文件 ≤800 行；
- 默认模型工具输入 Token 相对 Phase 6 基线下降 ≥35%；
- 大型工具输出输入上下文 Token 中位数下降 ≥40%；
- GoalContract 完成率不下降，干净目标误成功率不升高；
- 所有原始 Transcript、Artifact、Observation 和 Evidence lineage 可回读；
- 任何 hook/resource/tool proposal 都不能直接写 Evidence 或 Terminal；
- MCP 五个公开工具 schema 保持兼容；
- 每阶段独立提交、独立验收，失败不进入下一阶段。

## 删除优先原则

允许并鼓励删除代码，但采用“证明后删除”，不采用“为了兼容全部保留”：

- `OperationRuntimeAdapter`、重复 Runtime projection、重复 Planner/Scheduler/Workflow
  路径、默认低频 Worker 都是首批审查对象；
- 每个候选先锁定调用者、替代路径、恢复路径和 Evidence/Terminal 不变量；
- 先迁移测试，再删除引用，再删除导出，最后删除文件；
- shim 只能转发，最多保留一个版本周期；
- 代码减少必须伴随能力覆盖、终态准确率和回归时间数据；
- 详细候选矩阵见 `docs/architecture/lean-transparent-refactor.md`。

## 核心架构决策

1. 保留 SQLite/CAS/Lease 作为唯一事实源，不引入第二个可写 JSONL store。
2. 采用 Pi 的 session-tree 逻辑模型，但通过 `SessionJournal` 投影到现有持久层。
3. 保留一个最强主模型和一个 AgentLoop，不做固定 Specialist 流水线。
4. 工具默认 selected/catalog，强模型可显式 expand；不因省 Token 隐藏可用能力。
5. Projection、summary、report 都是导航投影，不能冒充 Evidence。
6. IDA 由用户自行通过 `ida` MCP preset 接入；不安装、不打包、不模拟 IDA Free。

## 迁移规则

- 旧 `OperationRuntime` 在迁移期间只保留兼容 facade，不能继续扩张调用面；
- `generic-adaptive` 作为兼容路径，L10 前不得删除；
- 每次删除前先添加调用者/恢复/攻击性回归测试；
- SQLite 只做向前、可重复、非破坏性迁移；
- Phase L2/L6 的 compaction 不删除原始记录；
- Phase L4/L5 的 Token 优化必须和能力覆盖、终态准确率一起验收；
- 前一阶段验收未通过时不得进入下一阶段。

## 阶段验收索引

```text
docs/acceptance/phase-0.md ... phase-6.md   历史基线
docs/acceptance/phase-6.1.md                Stateful MCP
docs/acceptance/phase-6.2.md                IDA Free feasibility
docs/acceptance/lean-L0.md                  Lean L0 complexity/behavior freeze
docs/acceptance/lean-l0-audit.json         Lean L0 machine-readable audit
docs/architecture/lean-transparent-refactor.md  L0–L11 详细方案
```
