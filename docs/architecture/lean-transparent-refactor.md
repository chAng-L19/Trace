# Lean Transparent Agent Refactor

## 目标

将 `codex-redteam-agent` 从“多个成熟子系统叠加的 Runtime”收敛为一个可解释、
可恢复、可扩展但不臃肿的强模型 Harness：

```text
一个 AgentLoop
一个 SessionJournal
一个 ToolRegistry
一个 ContextBudget
一个 EvidenceGate
多个可替换 Adapter
```

模型仍然负责战术搜索；Runtime 仍然负责作用域、凭据、预算、幂等、CAS、证据
lineage、清理和终态。轻量化只减少重复状态和重复编排，不减少安全不变量。

## 当前基线与问题

当前基线：

```text
HEAD: 1458ad9 chore: install frida runtime for reverse adapter
pytest: 307 passed, 1 skipped
compileall: passed
Python: 3.12 baseline
```

结构性问题：

1. `AgentService`、`OperationRuntime`、`OperationRuntimeAdapter` 同时承担应用编排。
2. `AdaptivePlanner`、`Scheduler`、`WorkflowRegistry`、`ToolBroker`、`WorkerManager`
   形成多重动作选择链，模型意图和 Runtime 动作容易重复表达。
3. Core contracts、legacy runtime models、adapter mappings 形成三套状态映射。
4. Context、Conversation、ModelLoop、ExplorationLedger 各自保存导航状态，存在重复摘要。
5. Tool schema、MCP preset、capability catalog、prompt projection 由不同位置控制。
6. 大输出虽已进入 CAS，但工具执行、投影、压缩之间仍缺少单一 bounded-output seam。
7. 旧 workflow 与新 tactical loop 并存时间过长，兼容面会变成永久编排面。

复杂度预算：重构完成后生产代码目标不超过 16,000 行、模块不超过 80 个、任何
单文件不超过 800 行；未达到预算的新增抽象必须有性能或不变量证据。

## 删除优先审计清单

以下是基于当前 import/调用搜索的候选，不是未经验证的直接删除列表：

| 候选 | 当前重复 | 删除门 |
|---|---|---|
| `adapters/runtime.py:196` `OperationRuntimeAdapter` | `AgentService` 已是 canonical facade，adapter 又包一层 start/run/status/cancel | 所有 `tests/test_runtime_core_adapter.py` 场景迁移到 AgentService contract test；旧调用只保留一个版本 shim |
| `adapters/runtime.py:63-194` 三个 Runtime*Adapter | Core DTO 与 legacy runtime 之间重复映射和双向状态转换 | `RuntimeStore/Event/ToolAdapter` 合并为单一 projection；CAS、event sequence、tool cancel 回归全部通过 |
| `runtime/adaptive_planner.py` + `runtime/scheduler.py` | 都参与 capability/action 选择；模型 tactical loop 已产生动态 proposal | 提取纯 `NextActionPolicy` 后删除重复路径；Terminal remediation、ensemble coverage、retry 语义必须有专测 |
| `runtime/workflow_registry.py` + `workflows/generic-adaptive.toml` | 只有一个固定 workflow，搜索空间已由模型主导 | L10 前保留兼容读取；所有新运行不再依赖固定动作链；旧快照和 MCP 调用继续通过 shim |
| `application/tactical_loop.py` 与 `model_loop.py` mixin | tactical/model loop 共享上下文但分裂控制流 | L3 合并为单一 AgentLoop；stream、parallel tool call、integrity 和 cancellation 测试迁移完成 |
| `application/context.py` 与 `conversation_*` / `exploration_*` 导航字段 | 多处保存摘要、selected messages、recon digest 和 branch 导航 | L2 由 SessionJournal 统一索引；原始 transcript、artifact、evidence 引用不得减少 |
| 默认 `docker`/`codex_handoff` Worker 构造 | 低频能力每次 AgentService 初始化都加载，但主 loop 不一定使用 | 改为显式 adapter registry/lazy load；不删除用户主动配置的 worker |
| `runtime/mcp_broker.py` 与 `tool_broker.py` 的生命周期重复 | discovery、run client、cleanup、catalog 刷新跨两个 Broker 层 | L9 只保留一条 transport/registry/cleanup 路径；MCP 五工具 schema 和 run isolation 必须不变 |

删除规则：

1. 先加“调用者存在/不存在”和“替代路径”等价测试，再移动或删除实现。
2. 先删除生产引用，再删除导出符号，最后删除文件；每一步可独立回滚。
3. compatibility shim 只能转发，禁止新增业务逻辑；超过一个版本周期必须删除。
4. 任何候选若仍被公开 API、fixture、迁移脚本或恢复逻辑使用，则标记为保留，不强删。
5. 删除后复杂度下降必须以模块数、行数、import 边界和测试时间实际测量，不以主观判断代替。

## 从 Pi 吸收的能力

参考版本：`earendil-works/pi`，commit `9767ba275f3e9a5ee0f5c5342249b629ab1b2282`，
范围：`packages/coding-agent`。

### 1. 透明 Session Tree

Pi 的 session manager 使用 append-only JSONL entries、`parentId` 和 current leaf；
分支、fork、resume、compact 都是显式 session entry，而不是隐藏内存状态。

本项目采用同一逻辑模型，但不替换当前 SQLite/CAS：SQLite 继续是权威持久层，新增
`SessionJournal` facade 将事件、消息、压缩边界、分支和 leaf 统一投影。不能引入第二
个可写事实源。

### 2. Tool Registry 与显式选择

Pi 的 system prompt 只展示 selected tools 及一行 tool snippet，未知工具不进入 active
registry。吸收为：

- `ToolRegistry` 只维护可用、已选择、side-effect、schema hash 和 source；
- default catalog 使用 capability/profile 选择，不把完整 MCP 工具集默认塞进 prompt；
- 强模型需要时可显式 expand，expand 本身记录事件和 token 成本；
- 完整 schema 仍可被模型使用，轻量化只作用于默认可见集合。

### 3. Incremental OutputAccumulator

Pi 维护滚动 tail、行数/字节双上限，超过上限才落临时文件，同时保留完整输出路径。
吸收为统一 `BoundedOutput` seam：

- display projection：默认 2,000 行/50 KiB，可按工具 profile 调整；
- raw artifact：始终 SHA-256 CAS；
- truncation metadata：total bytes/lines、截断原因、head/tail 方向；
- streaming 不在内存累计完整输出；
- Evidence 只能引用 raw artifact，不接受 projection 文本作为来源。

### 4. Compaction Boundary

Pi 明确区分 session entries 与 active context：compaction entry 替代旧上下文，但原始
entry 保留在 session tree；cut point 不切在 tool result 中间，保留 assistant tool call
与其结果的完整关系。

本项目吸收：

- 用 `context_window - reserve_tokens` 触发压缩；
- 优先在 user/assistant turn 边界切割；
- assistant tool call 与 tool result 作为不可分割组；
- 保留原始 transcript、Artifact、Evidence 引用和 prompt/response hash；
- 长度中断只记诊断 Artifact，不作为成功 checkpoint；
- 压缩失败最多一次可验证重试，预算连续性不重置。

### 5. Resource Loader 与 Skills

Pi 将项目资源、context files、skills、prompt templates 和 extensions 分离加载，并在
system prompt 中只注入已选资源。吸收为 `ResourceResolver`：

- `AGENTS.md`、项目 context、Capability Pack、MCP instructions 统一进入资源索引；
- 加载来源、优先级、hash、token 成本可查看；
- 未被当前任务选中的 skill/resource 不进入 prompt；
- resource 内容永远不是 Evidence，只有工具观察经过 verifier 才能晋升。

### 6. Session Lifecycle Hooks

Pi 的 session start/shutdown/before-switch/before-fork hooks 让扩展参与生命周期，但
扩展不能接管 session authority。本项目用有限事件 hook 替代更多 manager：

```text
session_started
before_run
before_tool_call
after_tool_observation
before_compaction
after_compaction
before_cancel
session_closed
```

hook 只读或产生经过 Runtime 校验的 proposal；不能直接写 SQLite、Evidence 或 Terminal。

## 明确不吸收的 Pi 特性

- 不用 JSONL 替换当前 SQLite CAS；CAS/Lease/CAS commit 是红队运行不变量。
- 不把 extensions 变成任意代码写 Evidence 的后门。
- 不用 TUI session 状态代替机器可验证 TerminalJudge。
- 不用“工具 schema 越少越好”牺牲强模型的可探索能力；默认隐藏可逆、显式展开。
- 不把 compaction summary 当作原始事实；summary 只是导航投影。
- 不引入 TypeScript/Bun、第三方 runtime 或新的持久化依赖。

## 目标结构

不是增加新框架，而是收敛入口：

```text
src/redteam_agent/
  core/                 # 稳定 DTO、contracts、ports；不含 runtime 实现
  application/
    agent_service.py    # 唯一公开应用入口
    agent_loop.py       # 单一 model -> tool -> observation loop
    context.py          # selector + compaction facade
  runtime/
    session_journal.py  # session tree facade over SQLite
    tool_registry.py    # catalog/selection/BoundedOutput seam
    evidence_gate.py    # observation -> evidence -> finding promotion
    operation_runtime.py # compatibility facade，最终只剩转发
  adapters/              # MCP/Codex/provider/worker adapters
```

删除方向：

- 将 `Scheduler`、`AdaptivePlanner` 中重复的动作选择逻辑收敛为 `NextActionPolicy`；
- 将 `OperationView`、runtime/core mapping 重复转换收敛为一次 projection；
- 将 Conversation/Exploration/ReconDigest 的导航字段由 SessionJournal 统一索引；
- 将 `ToolBroker` 的 discovery、selection、bounded output 分成可测试纯函数，但不再
  增加 manager 层；
- Phase 9 后删除不再被调用的 `generic-adaptive` 固定动作链和 legacy adapter 分支；
- Docker、Codex handoff 等低频能力保留 adapter，不进入主 loop 的默认构造路径。

## 阶段计划与验收

### L0：复杂度与行为冻结

动作：建立模块/行数/import/call-graph 报告；冻结当前 280 passed 基线、MCP schema、
状态转换、Evidence/Terminal 快照；为每个拟删除组件记录调用者。

验收：两次报告一致；无新行为；每个删除候选都有替代路径和回滚点。

当前结果：已通过。机器报告见 `docs/acceptance/lean-l0-audit.json`，验收记录见
`docs/acceptance/lean-L0.md`。当前不删除生产候选，下一阶段从 `OperationRuntimeAdapter`
兼容迁移开始。

### L1：唯一入口与边界收敛

动作：`AgentService` 成为唯一应用入口；`OperationRuntime`、MCP、CLI、Codex 只做
facade；禁止新增直接调用 Runtime 内部 mixin 的代码；建立 `AgentService` contract test。

验收：所有入口走同一 start/run/status/cancel/events；并发/CAS/lease/取消竞争回归通过；
旧公开 API 等价；主 loop 不超过一个。

当前结果：已通过。CLI、MCP production path 和旧 Runtime adapter 均通过
`AgentService`；fake-runtime fallback 仅用于协议测试。`OperationRuntimeAdapter` 已降为
转发 shim，Store/Event/Tool adapter 的删除延后到 L2/L4。

### L2：SessionJournal 与透明树（已通过）

动作：把 transcript、exploration、recon digest、model request/observation、events
统一为 run-bound journal projections；增加 parent/leaf/branch、session info、raw
entry refs；SQLite 仍是唯一写入源。

验收：任意时点可导出完整 session tree；resume/fork/branch/replay 结果稳定；原始消息
和 Artifact 不丢失；跨 run 引用被拒绝；Journal 不直接晋升 Evidence。

当前结果：`SessionJournal` 已覆盖 session tree、branch/leaf、transcript 和事件投影，
对应恢复、分支和不可变记录测试通过。

### L3：单一 AgentLoop（已通过）

动作：把 ModelLoop/TacticalLoop/部分 Scheduler 路径合并为一个明确循环：

```text
select context → request model → validate proposal → execute tool/worker
→ persist observation → verifier proposal → next turn
```

Planner 只生成 `NextActionProposal`，Runtime 只验证 proposal；不再存在 Runtime 预排的
固定动作 DAG。

验收：强模型可在一个 gate 内连续搜索；动态分支/重开保持；预算、幂等、Lease、Evidence
语义不变；相同 Fake Provider 测试覆盖文本/structured/tool/parallel/stream/cancel。

当前结果：模型循环和战术循环已收敛为同一应用路径，Fake Provider、流式、并行工具、
取消和恢复测试通过。

### L4：ToolRegistry 与开源工具直接集成（已通过）

动作：将 MCP/builtin/worker tool 统一进入 registry；默认只传 selected definitions 和
一行 snippet；提供 `tools.expand`/profile；记录 tool source、schema hash、catalog
revision、visibility reason。

验收：默认 prompt tool token 下降至少 30%；expand 后能力完整；未知 tool 不可调用；
tool-list change 可恢复；side-effect annotation 不可由模型覆写。

当前结果：默认运行时直接注册 16 个开源工具，Playwright/Capstone/Frida 使用 Python
API，radare2/Rizin 和云 CLI 使用内置受控 Adapter；无 IDA/IDB 能力残留，工具目录、
副作用标记、schema hash 和回归测试通过。

### L5：BoundedOutput 与 Artifact streaming（下一阶段）

动作：实现 Pi 风格 `BoundedOutput`：head/tail、行数/字节双限制、增量 decoder、raw
CAS 文件、truncation metadata；所有 Local/MCP/Codex/HTTP 输出统一经过该 seam。

验收：大输出不造成 OperationState 增长；projection 永不替代 raw artifact；UTF-8 边界、
二进制、超时、取消和进程崩溃均可恢复；工具结果 token 中位数下降至少 40%。

### L6：Context Budget 与可追溯 Compaction

动作：引入 context window/reserve/keep-recent；只在 turn boundary 压缩；保留 tool-call
组、Goal 条款、活动分支、关键 Evidence refs、未验证假设和清理义务；摘要带 source hash。

验收：压缩不删除原始 journal；上下文溢出最多一次可验证重试；usage 缺失不伪造；
中断输出不成 checkpoint；等模型/目标/预算下输入 token 下降至少 35%，完成率不下降。

### L7：ResourceResolver 与透明扩展

动作：引入项目资源/skill/Capability Pack/MCP instruction loader；生命周期 hook 只有
proposal 权限；每个资源输出来源、优先级、hash、token cost；默认不把全部 skills 注入。

验收：资源可解释、可禁用、可复现；恶意/错误 resource 不能改变 scope、Evidence 或
terminal；同一 cwd/session 两次加载 hash 一致；resource token 占比受预算约束。

### L8：EvidenceGate 收敛

动作：把 verifier、builtins、TerminalJudge 的重复晋升检查收敛到一个 EvidenceGate；
Finding 必须引用 raw observation、影响证明、负向控制、目标和清理证明；report 变成纯投影。

验收：孤立 Artifact、伪造报告、错误目标、缺父证据、缺影响/清理/negative control 均不能
成功；每条 GoalContract predicate 都可反向追溯到 raw Observation。

### L9：MCP/Worker 适配器瘦身

动作：保持现有 Playwright run-scoped MCP；将 transport、registry、worker execution
和 lifecycle cleanup 只保留一条路径；Docker/Codex 为显式 adapter，默认不加载。

验收：五个公开 MCP 工具 schema 兼容；Playwright 真实 fixture；内置开源工具验收；worker
重启、取消、幂等、workspace/credential 隔离通过；没有隐藏全局 client。

### L10：删除旧编排与发布门

动作：删除无调用 legacy mapping、固定 generic workflow、重复 context/index、兼容期限到期
的 facade；生成复杂度报告和迁移说明；保留必要 shim 一个版本周期。

验收：生产文件 ≤80、代码 ≤16,000 行、单文件 ≤800 行；全量回归、快照、wheel、隔离安装、
self-test、MCP initialize/tools/list 全部通过；旧调用方有明确迁移路径。

### L11：透明度与成本评测

动作：增加 `session inspect/export`、`events --jsonl`、tool visibility explain、context
usage、compaction boundary、evidence lineage 命令；固定 10 个 Web/API 与 5 个干净目标。

验收：每个模型动作可解释；输入 token、工具 token、raw artifact、Evidence lineage 和终态
都能导出；相同模型/预算对比 Phase 6 基线，token 至少下降 35%，完成率不下降，误成功率不升高。

## 迁移顺序与硬门

严格顺序：`L0 → L1 → L2 → L3 → L4/L5 → L6 → L7 → L8 → L9 → L10 → L11`。

- 每阶段单独提交与 `docs/acceptance/lean-LN.md`；
- 前一阶段未通过不得进入下一阶段；
- 每次删除先加调用路径测试，再删实现；
- 任何 token 优化必须同时报告能力覆盖和终态准确率；
- 任何扩展/资源机制不得拥有 Runtime authority；
- 不以“报告生成成功”“工具返回 success”“模型声称完成”作为验收证据。

## 参考映射

| Pi 能力 | 本项目落点 | 保留边界 |
|---|---|---|
| append-only JSONL session tree | `SessionJournal` over SQLite | SQLite/CAS 是唯一事实源 |
| selected tools + snippets | `ToolRegistry` | 强模型可显式 expand |
| `OutputAccumulator` | `BoundedOutput` | raw CAS 永不截断 |
| compaction cut point | `ContextBudget` | 不切 tool call/result 组 |
| resources/skills loader | `ResourceResolver` | 资源不是 Evidence |
| lifecycle hooks | runtime events/proposals | hook 不可写 terminal |
| session fork/resume | run branch/leaf | CAS/Lease/target scope 不变 |
