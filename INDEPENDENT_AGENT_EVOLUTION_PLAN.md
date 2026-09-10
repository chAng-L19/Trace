# Trace 第一性原理演进计划

技术兼容标识：仓库 `codex-redteam-agent`，Python 包 `redteam_agent`。
MCP `serverInfo.name` 继续使用 `redteam-agent-runtime`，作为兼容协议标识。

> 本文件是当前权威计划；`docs/architecture/lean-transparent-refactor.md` 只提供
> 架构背景和参考映射。每个阶段完成时，必须在同一提交中更新本文件的状态、验收
> 证据、基线和下一阶段入口。

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
| Capstone / radare2 / Rizin / Frida | 开源工具适配 | 反汇编、二进制元数据、字符串、动态进程和控制流分析 |

## 已完成基线

### Phase 0–5 与 L5

已完成：基线冻结、Core Contracts、AgentService 生命周期、Provider-agnostic ModelLoop、
Transcript/Context/Budget、Worker Plane、CAS Artifact Store、Lease/Fencing、幂等和恢复。
L5 已在独立分支 `feat/l5-bounded-output` 完成，并已通过 PR #2 合并到 `main`。

### Phase 6

已完成：模型主导的薄战术循环、ExplorationLedger、ObservedMiss/VerifiedNegative、
分支重开、ReconDigest、有界工具投影和完整 Artifact。

### Phase 6.1

已完成：run-scoped Playwright MCP、roots、工具目录刷新、取消、资源清理、
`mcp-doctor`、Token-efficient capability catalog，以及默认注册的开源工具 Adapter。

当前基线：

```text
pytest: 315 passed, 1 skipped
compileall: passed
wheel: built
self-test: terminal success
MCP initialize/tools/list: passed; 5 public tools
GitHub Actions: `.github/workflows/ci.yml` added for push/PR validation on Python 3.11–3.13
```

## 当前总计划

| 阶段 | 目标 | 状态 |
|---|---|---|
| L0 | 复杂度/调用图/行为冻结 | 已通过 |
| L1 | 唯一 AgentService 入口与边界收敛 | 已通过 |
| L2 | SessionJournal 透明会话树 | 已通过 |
| L3 | 单一 AgentLoop | 已通过 |
| L4 | ToolRegistry 与开源工具直接集成 | 已通过 |
| L5 | BoundedOutput 与流式 Artifact | 已通过 |
| L6 | ContextBudget 与可追溯 Compaction | 已通过（结构验收） |
| L7 | ResourceResolver 与透明扩展 | 已通过 |
| L8 | EvidenceGate 收敛 | 待执行 |
| L9 | MCP/Worker 适配器瘦身 | 部分完成 |
| L10 | 删除旧编排与发布门 | 待执行 |
| L11 | 透明度、Token 与能力评测 | 待执行 |

## 阶段交付与验收

### L0：复杂度与行为冻结（已通过）

交付：模块/行数/import/call-graph 审计、Phase 0–6 快照、删除候选矩阵、独立本地
Git 基线和恢复标签。

验收：审计连续两次内容一致；历史快照和状态转换不变；每个删除候选有调用者、替代
路径、恢复路径；`pytest`、`compileall`、wheel、隔离安装、self-test、MCP
initialize/tools/list 均通过。

### L1：唯一 AgentService 入口（已通过）

交付：CLI、MCP、Codex 和兼容 Adapter 统一调用 `AgentService.start/run/status/
cancel/events`；`OperationRuntime` 只保留兼容 facade。

验收：所有生产入口路径相同；CAS/Lease/幂等/取消竞争和多目标隔离回归通过；旧
调用方行为等价；不存在第二条生产编排链。

### L2：SessionJournal 透明会话树（已通过）

交付：把 transcript、model request/observation、exploration、recon digest、event
和 branch/leaf 投影到统一 Journal；SQLite 继续是唯一事实源。

验收：可导出完整 session tree；resume/fork/branch/replay 结果稳定；原始消息、
Artifact、Evidence 引用不丢失；跨 run 引用被拒绝；Journal 不能直接晋升 Evidence。

### L3：单一 AgentLoop（已通过）

交付：收敛为 `select context → request model → validate proposal → execute
tool/worker → persist observation → verify → next turn`；模型负责战术搜索，Runtime
负责不变量。

验收：Fake Provider 覆盖文本、结构化输出、串行/并行工具调用、流式中断、重试、
取消和恢复；动态分支/重开保持；预算、Lease、幂等、Evidence、Terminal 语义不变；
不存在固定角色流水线限制模型搜索。

### L4：ToolRegistry 与开源工具直接集成（已通过）

交付：MCP、builtin、worker 工具统一进入 Registry；默认只选择必要 definitions；
强模型可显式 expand；`source/schema_hash/revision/visibility` 可追溯。默认运行时
直接注册 HTTP、Playwright、DNS、TCP、Capstone、二进制解析、字符串、Frida、
radare2/Rizin、源码搜索、Python AST 和云清单工具，不依赖 MCP 配置；删除全部
IDA/IDB 能力。

验收：默认工具输入 Token 相对 L4 前下降至少 30%；expand 后能力完整；未知工具不可
调用；tools-list 变化可恢复；副作用标注不可被模型覆盖；16 个内置开源工具在确定性
fixture 上通过；IDA/IDB 搜索无结果；MCP 五个公开工具 schema 保持兼容。

### L5：BoundedOutput 与流式 Artifact（已通过）

交付：实现统一 `BoundedOutput` seam，采用 head/tail、行数/字节双限制、增量解码、
原始输出 SHA-256 CAS 和截断元数据；Local/MCP/Codex/HTTP 输出全部经过该 seam。

验收：大输出不会令 `OperationState` 无界增长；raw Artifact 始终完整且可回读；
projection 不冒充 raw；UTF-8 边界、二进制、超时、取消、进程崩溃和重复恢复均通过；
工具结果输入 Token 中位数下降至少 40%，能力覆盖和终态准确率不下降。

当前结果：`BoundedOutput` 统一模型流、工具结果、Local Worker 和 MCP Worker 的输出处理，
使用增量 JSON 编码、UTF-8 增量解码、head/tail、行数/字节双限制、SHA-256 和截断原因；
完整内容写入 CAS，SQLite/Transcript 只保存有界 projection。新增 UTF-8 分片、二进制、
超大工具结果、Local Worker 完整 stdout 和 CAS 回读测试；L5 合并后的全量回归为 315 passed, 1 skipped。
当前 L5 验收中的“工具结果输入 Token 中位数下降 40%”需要 L11 固定评测集完成统计，
本阶段已验证 projection 有界和原始能力不丢失。

### L6：ContextBudget 与可追溯 Compaction（已通过，固定成本指标留至 L11）

交付：统一 context window、reserve、keep-recent、action/token/time 预算；只在
turn boundary 压缩；保护 Goal、未满足条款、活动分支、关键 Evidence、未验证假设和
清理义务；摘要携带来源 hash。

验收：压缩不删除原始 Journal；tool-call/result 不被拆开；usage 缺失不伪造；半截
流只进入诊断 Artifact；溢出最多一次可验证重试；相同模型/目标下输入 Token 下降至少
35%，GoalContract 完成率不下降。

当前结果：新增 `ContextBudget` 统一 context window、输出预留、keep-recent、估算比例和
压缩重试策略；压缩仅在模型 turn boundary 或显式 context selection boundary 执行，普通
投影路径不会隐式晋升摘要。请求组按 `request_id` 全局聚合，assistant tool-call 与 tool-result 即使被交错写入
也保持原子；已纳入摘要来源的消息不会重复压缩。Provider 返回上下文溢出时最多进行一次
强制压缩重试，请求元数据记录 retry 和 compaction IDs；原始 Journal、Artifact、Evidence
和半截流诊断路径不变。新增 4 个上下文预算/原子性/重试测试，更新 Phase 5 contract
snapshot；全量回归为 `315 passed, 1 skipped`，compileall、pip check、wheel、self-test
均通过。固定评测集上的 35% Token 指标仍留到 L11，不用单个 fixture 推断中位数。

### L7：ResourceResolver 与透明扩展（已通过）

交付：统一加载 `AGENTS.md`、项目 context、skill、Capability Pack 和 MCP instructions；
记录来源、优先级、hash、token cost；hook 只能产生 Runtime 校验的 proposal。

验收：资源加载可解释、可禁用、可复现；错误或恶意资源不能改变 scope、Evidence 或
Terminal；同一 cwd/session 两次 hash 一致；未选资源不进入 prompt；资源 Token 受预算
约束。

当前结果：新增 `ResourceResolver`，统一索引 `AGENTS.md`、项目 context、skill、
Capability Pack 和 MCP instruction；每项资源记录来源、优先级、UTF-8 内容 hash、字节数
和估算 Token 成本。默认只选择 `agents` 与 `project_context`，其余资源必须显式匹配；
支持禁用 pattern、Token budget、缺失/编码/大小/符号链接诊断，并用确定性 index/selection
hash 固定选择结果。资源只进入 ContextSelector 的 system projection 和 ModelRequest
metadata，不能直接写入 Scope、Evidence 或 Terminal。Resource metadata 纳入 Context
snapshot、source hash 和 context metrics，资源投影不重复写入 Transcript。

L7 验收结果：目标资源测试 24 项通过；全量回归 317 passed、1 skipped；compileall、
pip check、Phase 5 snapshot check、wheel、source self-test 和 MCP initialize/tools/list
全部通过；生产 Python 模块均不超过 800 行。固定评测集上的 Token 中位数与模型能力指标
仍按 L11 统一测量，本阶段不虚构统计结论。

### L8：EvidenceGate 收敛（待执行）

交付：将 verifier、builtins、Finding 和 TerminalJudge 的重复晋升检查收敛为单一
EvidenceGate；报告仅作投影。

验收：孤立 Artifact、伪造报告、错误目标、缺父证据、缺影响证明、缺负向控制或缺清理
证明均不能成功；每个 GoalContract predicate 都能反向追溯至原始 Observation。

### L9：MCP/Worker 适配器瘦身（部分完成）

交付：保留通用 run-scoped MCP、Local/MCP/Codex/Docker Worker adapter；收敛 transport、
registry、执行、取消、重启和 cleanup 为单一路径；低频 Worker 延迟加载。

验收：MCP 五工具 schema、Playwright fixture、worker 重启/取消/幂等、workspace/凭据
隔离全部通过；无隐藏全局 client；工具结果自动回填 Observation；断线和重复提交不
重复副作用。

### L10：删除旧编排与发布门（待执行）

交付：按删除优先矩阵移除无调用 mapping、固定 generic workflow、重复 context/index
和过期 shim；保留必要 shim 一个版本周期并生成迁移说明。

验收：生产 Python 文件不超过 80、代码不超过 16,000 行、单文件不超过 800 行；全量
回归、快照、wheel、隔离安装、self-test、MCP initialize/tools/list 全通过；每个旧
入口有明确迁移路径。

### L11：透明度与成本评测（待执行）

交付：`session inspect/export`、事件流、tool visibility explain、context usage、
compaction boundary 和 Evidence lineage 查询；固定 Web/API 评测集与干净目标集。

验收：每个模型动作、工具选择、Token、Artifact、Evidence 和终态可导出；相同模型/预算
相对 Phase 6 基线 Token 至少下降 35%；GoalContract 完成率不下降；5 个干净目标零错误
终态成功；结果可连续复现。

## 执行规约与 Ponytail

- 本对话持续启用 Ponytail `full`：先复用现有实现，再用标准库/平台能力；删除优先；
  不增加只有一个实现的抽象；每个非平凡逻辑保留一个最小可运行检查。
- Ponytail 只压缩重复状态、重复映射、无调用兼容层和无收益依赖，不削弱输入校验、
  错误处理、作用域、凭据、幂等、CAS、Lease、Evidence lineage、清理或终态裁决。
- 每次阶段完成必须同时完成四项：更新本文件；补充或更新机器可验证测试；记录实际
  基线和验收结果；单独提交。未通过验收不得把阶段标为完成，也不得进入下一阶段。
- 阶段报告只陈述实际改动、证据和剩余缺口；不把计划、模型输出、报告文本或工具
  `success` 标志当作完成证据。

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
6. 逆向能力优先使用内置 Capstone、二进制解析、字符串提取、Frida 和 radare2/Rizin
   Adapter；外部 MCP 只作为可选扩展，不是默认能力来源。

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
docs/acceptance/phase-6.1.md                Open-source Capability Plane
docs/acceptance/lean-L0.md                  Lean L0 complexity/behavior freeze
docs/acceptance/lean-l0-audit.json         Lean L0 machine-readable audit
docs/acceptance/lean-L1.md                  Lean L1 canonical entry acceptance
本文件“阶段交付与验收”                        L0–L11 当前权威状态与验收标准
docs/architecture/lean-transparent-refactor.md  架构背景与参考映射
```
