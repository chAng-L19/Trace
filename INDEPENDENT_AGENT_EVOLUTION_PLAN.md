# Codex Red-Team 独立 Agent 演进方案

## 文档状态

- 新目录：`E:\cli\codex-redteam-agent`
- 本文性质：独立 Agent 架构与迁移方案
- 当前实现状态：已冻结 `v2.1.0` 基线，并完成首批独立 Runtime、统一工作流、MCP 入口与测试迁移
- 目标：构建宿主无关 Agent Kernel，Codex 降为第一套 Host Adapter
- 约束：轻量、单 Agent、SQLite、无技能库、无 RAG、无知识库、无固定领域 Agent

---

## 1. 方案结论

本方案从一开始将 Codex、App、CLI、Hook、MCP 视为外部适配器：

```text
Independent Agent Kernel
  ├── Prompt Compiler
  ├── Goal / Plan / Scheduler
  ├── Executor / ToolBroker
  ├── Fact / Evidence / Review
  ├── TerminalJudge
  └── SQLite Event Store
          ↑
    Ports / Adapters
          ↑
  Codex / CLI / API / Local Model / MCP
```

未来独立 Agent 可直接调用模型 Provider 和工具，不再依赖：

```text
$CODEX_HOME
hooks.json
model_instructions_file
Codex App Server
Codex Runtime MCP control plane
```

Codex 适配器仍保留，用于兼容现有用户和回归对照。

---

## 2. 目标目录

```text
codex-redteam-agent/
├── pyproject.toml
├── src/
│   └── redteam_agent/
│       ├── core/
│       │   ├── domain/
│       │   │   ├── batch.py
│       │   │   ├── budget.py
│       │   │   ├── goal.py
│       │   │   ├── message.py
│       │   │   ├── plan.py
│       │   │   ├── fact.py
│       │   │   ├── evidence.py
│       │   │   ├── review.py
│       │   │   └── event.py
│       │   ├── application/
│       │   │   ├── batch_service.py
│       │   │   ├── conversation.py
│       │   │   ├── service.py
│       │   │   ├── model_loop.py
│       │   │   ├── scheduler.py
│       │   │   ├── executor.py
│       │   │   ├── replanner.py
│       │   │   ├── verifier.py
│       │   │   └── terminal.py
│       │   └── ports/
│       │       ├── model.py
│       │       ├── tool.py
│       │       ├── store.py
│       │       ├── event.py
│       │       └── host.py
│       ├── prompting/
│       │   ├── assembler.py
│       │   ├── manifest.py
│       │   ├── contracts.py
│       │   ├── core/
│       │   ├── models/
│       │   ├── scenes/
│       │   ├── runtime_bindings/
│       │   └── priming/
│       ├── adapters/
│       │   ├── models/
│       │   │   ├── openai_compatible.py
│       │   │   └── host_model.py
│       │   ├── tools/
│       │   │   ├── mcp.py
│       │   │   ├── local.py
│       │   │   └── host.py
│       │   ├── storage/
│       │   │   └── sqlite.py
│       │   ├── hosts/
│       │   │   ├── codex/
│       │   │   └── standalone/
│       │   └── transports/
│       │       ├── mcp_server.py
│       │       └── local_api.py
│       ├── workflows/
│       │   ├── generic-adaptive.toml
│       │   └── profiles.toml
│       └── entrypoints/
│           ├── agent_main.py
│           └── codex_main.py
└── tests/
    ├── batch/
    ├── core/
    ├── model_loop/
    ├── prompts/
    ├── adapters/
    ├── recovery/
    └── prompt_bank/
```

核心依赖规则：

```text
core -> Python standard library only
prompting -> core contracts
adapters -> core ports
entrypoints -> application service + adapters
core -X-> Codex/MCP/WebSocket/tomlkit
```

---

## 3. Agent Kernel

### 3.1 AgentService

```python
class AgentService:
    def start(self, request): ...
    def run(self, run_id): ...
    def submit_observation(self, run_id, observation): ...
    def status(self, run_id): ...
    def cancel(self, run_id, reason): ...
    def events(self, run_id, after_sequence=0): ...
```

当前 Codex Runtime MCP 调用该对象；独立 Agent 入口直接调用该对象。

### 3.2 Ports

```python
class ModelPort:
    def capabilities(self): ...
    def complete(self, request): ...
    def stream(self, request): ...
    def cancel(self, request_id): ...

class ToolPort:
    def discover(self): ...
    def invoke(self, call): ...
    def reconcile(self, idempotency_key): ...
    def cancel(self, call_id): ...

class StorePort:
    def load_operation(self, run_id): ...
    def commit_transition(self, transition): ...

class EventPort:
    def append(self, event): ...

class HostPort:
    def capability_snapshot(self): ...
    def emit_status(self, status): ...
```

### 3.3 Host Worker

当前 Codex 模式需要模型按照 `next_action_spec` 调用 Host 工具。未来抽象为：

```python
class HostWorkerPort:
    def execute(self, spec, context): ...
```

实现：

```text
CodexPromptHandoffWorker
StandaloneModelToolWorker
```

这样 Host-only action 在独立 Agent 中可以程序化执行，不需要依赖模型从文字 Prompt 中主动回灌。

### 3.4 非回归能力基线

从 Codex Runtime 抽取内核时必须原样保留以下语义，不能用“独立 Agent 重写”为理由退回单目标、单轮或非持久执行：

```text
Multi-target batch
Action/Token/Time budget + paused_budget
Host handoff without user relay
Cancel/resume + cleanup proof
Operation CAS
Lease fencing
Atomic Attempt/Observation/Evidence/State commit
```

Multi-target 由 `BatchService` 管理：每个目标拥有独立 `run_id`、GoalContract、PlanRevision、Fact/Evidence namespace 和 TerminalDecision；批次只保存成员关系、调度策略及聚合终态。跨目标复用发现结果时必须建立显式 Evidence lineage，不能共享可变状态。

### 3.5 Provider-agnostic ModelLoop

独立 Agent 的模型循环属于 Application 层，不放进 Provider Adapter，也不由 Prompt 隐式驱动：

```text
load Run + Conversation
  -> negotiate ProviderCapabilities
  -> assemble one System PromptBundle
  -> select context window
  -> model complete/stream
  -> validate assistant output and tool calls
  -> dispatch through ToolBroker
  -> append tool Observation
  -> Verifier / Replanner / TerminalJudge
  -> compact context if required
  -> continue / paused_budget / cancelled / terminal
```

Provider 能力协商使用统一对象：

```python
ProviderCapabilities(
    native_system_role: bool,
    native_tool_calls: bool,
    parallel_tool_calls: bool,
    structured_output: bool,
    streaming: bool,
    usage_reporting: bool,
    max_context_tokens: int,
)
```

规则：

1. Provider Adapter 只报告能力并转换协议，不拥有 Goal、Plan、预算或终态。
2. 原生 Tool Call 可用时使用结构化调用；不可用时由 Runtime Binding 生成可校验的结构化调用协议。
3. Parallel Tool Calls 只有在依赖图无冲突、执行策略允许且幂等边界明确时启用。
4. Provider 切换不改变 GoalContract、Evidence 语义和 TerminalJudge。
5. 每个模型请求记录 PromptBundle Hash、Provider/Model ID、capability snapshot、usage 和 response hash。

### 3.6 ConversationManager、压缩与预算

ConversationManager 持久化完整的 `system/user/assistant/tool` Transcript。每条消息至少包含：

```python
MessageRecord(
    message_id,
    run_id,
    role,
    origin,
    content_ref,
    content_hash,
    correlation_id,
    parent_message_id,
    token_count,
    evidence=False,
)
```

Tool 输出以 `role=tool` 回填，但默认仍是 Observation；只有 Verifier 能将其提升为 Fact/Evidence。Priming History、摘要和模型自述均保持 `evidence=false`。

上下文压缩规则：

- 永不压缩掉 System Core、当前用户原始目标、未满足 Goal clauses、活动 Plan、关键 Evidence 引用和不可逆操作状态。
- 摘要保存父消息范围、源 Hash、摘要模型/版本和生成时间，可回溯到原始 Transcript。
- 压缩只影响下一轮模型输入，不删除权威事件、Artifact 或原始消息。
- 达到上下文上限而无法安全压缩时进入可恢复暂停，不静默截断。

统一预算：

```python
RunBudget(
    action_limit,
    token_limit,
    time_limit_seconds,
    actions_used,
    input_tokens_used,
    output_tokens_used,
    started_at,
    deadline,
    pause_reason,
)
```

`paused_budget` 是持久可恢复状态。`resume` 接受 BudgetDelta 或继续使用尚未耗尽的维度；预算耗尽不得被 TerminalJudge 当作成功。取消信号在模型请求、流式响应、工具调用和批次子 Run 之间传播；流式中断产生的半截消息只保存为诊断 Artifact，不进入事实链。完成/取消竞争由 Operation CAS、Lease fencing 和 atomic commit 决定唯一有效提交。

---

## 4. codex1 完整破限方向

### 4.1 迁移原则

codex1 的有效部分被拆成四类：

```text
System behavior contract
Intent/context compiler
Execution policy
Host adapter behavior
```

不把 217 行 AGENTS、564 行 Hook 和全局权限配置整体复制进新 Agent。

### 4.2 能力映射

| codex1 能力 | 独立 Agent 位置 |
|---|---|
| 研究身份、运行域 | Prompt Core |
| Context over Keywords | Intent Fidelity Contract |
| Default Assistance | Prompt Core + Model Profile |
| Reinterpret Ambiguity | IntentCanonicalizer |
| Execution Fidelity | Runtime Contract + TerminalJudge |
| Findings as Deliverables | Finding/Evidence domain types |
| 八类场景 | Scene Profiles |
| LLM/关键词分类 | IntentClassifier Port |
| Follow-up TTL | Conversation Binding in SQLite |
| 4–6 轮强化历史 | PrimingProfile |
| 高权限配置 | ExecutionPolicy |
| Hook additionalContext | Codex Host Adapter |
| 固定工具栈 | Runtime Capability Manifest |

### 4.3 无损意图编译

```python
@dataclass(frozen=True)
class IntentEnvelope:
    original_text: str
    intent_hash: str
    scene: str
    action_kind: str
    execution_required: bool
    verbs: tuple[str, ...]
    targets: tuple[str, ...]
    deliverables: tuple[str, ...]
    constraints: tuple[str, ...]
    confidence: float
```

IntentClassifier 不拥有计划、状态或终态，只输出结构化上下文。

---

## 5. Prompt Compiler

### 5.1 PromptBundle

```python
@dataclass(frozen=True)
class PromptBundle:
    system_text: str
    runtime_context: dict
    priming_history: tuple[Message, ...]
    model_profile: str
    host_binding: str
    manifest_hash: str
    fragment_hashes: dict[str, str]
```

### 5.2 Prompt 层级

```text
L0 Preserved User System
L1 Security Research Core
L2 Runtime Binding
L3 Exactly One Model Profile
L4 Session/Operation Context
L5 Scene/Intent Overlay
L6 Optional Priming History
```

### 5.3 Core Fragments

```text
prompting/core/
├── research-identity.md
├── operating-domain.md
├── intent-fidelity.md
├── execution-contract.md
├── evidence-contract.md
└── completion-contract.md
```

内容覆盖：

- 上下文优先于关键词。
- 保留完整复合意图。
- 执行型任务不得降级。
- 工具输出和事实分层。
- 自动重试、替代和重规划。
- 发现立即留档。
- 目标谓词、覆盖、清理和报告终态。
- 不可逆工程 Gate。

### 5.4 Runtime Binding

```text
runtime_bindings/
├── codex.md
└── standalone.md
```

Prompt Core 不知道 `redteam_run`、MCP 或 `$CODEX_HOME`。Codex Binding 提供现有工具名，Standalone Binding 提供原生 AgentService/ToolRegistry 语义。

### 5.5 模型 Profile

每个 Agent Session 只选择一个 Profile：

```text
models/Jailbreak.gpt-5.4.md
models/Jailbreak.gpt-5.5.md
models/Jailbreak.gpt-5.6.md
models/Jailbreak.default.md
```

Profile 只描述模型差异，不重复核心任务域、工具协议和完成语义。

### 5.6 Priming

```text
priming/execution-bootstrap-v2.json
```

独立 Agent 直接将消息加入 ConversationStore。Priming 消息标记为 `origin=bootstrap`、`evidence=false`，不得作为事实或完成证据。

---

## 6. ExecutionPolicy

统一宿主无关执行策略：

```python
@dataclass(frozen=True)
class ExecutionPolicy:
    approval: str
    filesystem: str
    network: str
    irreversible_confirmation: bool
```

Profile：

```toml
[execution_profiles.inherit]
approval = "host-managed"
filesystem = "host-managed"
network = "host-managed"

[execution_profiles.workspace]
approval = "on-risk"
filesystem = "workspace"
network = "restricted"

[execution_profiles.unrestricted]
approval = "never"
filesystem = "unrestricted"
network = "unrestricted"
irreversible_confirmation = true
```

映射：

```text
Codex Adapter      -> approval_policy / sandbox_mode
Standalone Adapter -> LocalExecutor policy
Remote Agent       -> Server-side execution policy
```

不在 Prompt 内修改执行权限，也不新增目标级预授权配置开关。

---

## 7. Plan、Fact、Review 与恢复

### 7.1 PlanRevision

```python
PlanRevision(
    plan_id,
    revision,
    parent_revision,
    tasks,
    edges,
    created_from,
)
```

Planner 只输出 `PlanDelta`，不原地修改已执行 Plan。

### 7.2 FactRecord

```python
FactRecord(
    fact_id,
    run_id,
    branch_id,
    key,
    value,
    status,
    version,
    provenance_evidence_ids,
    produced_by_attempt,
    invalidated_by,
)
```

Ready 判定依据 Task 依赖和 Fact 版本，而不是只看父 Action 是否 `completed`。

### 7.3 ReviewDecision

```python
ReviewDecision(
    review_id,
    scope,
    evidence_refs,
    verdict,
    blockers,
    reviewer_version,
)
```

Review 只能阻止或要求修订，不能把未验证观察直接变成事实。

### 7.4 崩溃恢复

外部调用后未提交结果：

```text
running -> uncertain -> reconcile/probe -> completed/retry
```

Lease 使用 fencing token；Operation 使用乐观版本 CAS。

---

## 8. 事件协议

不引入 Kafka、Redis 或独立消息服务。SQLite 保存 append-only Event Log，EventBus 首期为进程内同步订阅器。

```json
{
  "schema_version": 1,
  "event_id": 128,
  "type": "task.observation_received",
  "run_id": "run-...",
  "session_id": "session-...",
  "branch_id": "main",
  "sequence": 31,
  "actor": {"kind": "tool", "id": "mcp:browser"},
  "correlation_id": "action-4",
  "causation_id": "event-127",
  "occurred_at": "...",
  "payload_hash": "...",
  "payload": {}
}
```

事件：

```text
goal.compiled
plan.revised
task.ready
task.dispatched
task.started
task.observation_received
task.reconciled
fact.asserted
fact.invalidated
evidence.promoted
gate.passed
gate.failed
review.accepted
review.revise
operation.completed
operation.cancelled
```

---

## 9. SQLite 权威状态

建议表：

```text
operations
goal_contracts
plan_revisions
tasks
task_attempts
facts
evidence_nodes
artifacts
events
leases
reviews
host_bindings
conversation_bindings
prompt_bundles
```

规则：

- Operation 行带 `version`，所有写入使用 CAS。
- Lease 带 fencing token。
- 大型输出写入按 Hash 寻址 Artifact Store。
- Snapshot 是派生缓存，可以从 Event Log 重建。
- Hook JSON 不保存 Operation、Plan 或 Workflow 状态。
- PromptBundle 版本和 Hash 写入 Run 元数据。

---

## 10. ToolBroker

ToolBroker 分为：

```text
Core ToolSelector
ToolPort
MCPToolAdapter
LocalToolAdapter
HostToolAdapter
```

选择依据：

```text
declared capabilities
input/output schema
health
latency
semantic success rate
priority
current execution policy
```

幂等键：

```text
hash(run, branch, plan_revision, task, tool, normalized_input, fact_versions)
```

外部 MCP 自动发现保留。项目不内置扫描器、攻击技能或固定工具包。

---

## 11. Codex Adapter

Codex 适配器负责：

```text
model_instructions_file
SessionStart/UserPromptSubmit Hook
CLI Launcher
App 兼容逻辑
Runtime MCP transport
AGENTS compatibility layer
installation/upgrade/uninstall
```

Codex Hook 只负责：

```text
命令解析
模型/session 标识
场景提示
run binding
紧凑 ContextEnvelope
```

Codex Runtime MCP 调用 `AgentService`，不在 MCP handler 内实现业务状态机。

---

## 12. Standalone Adapter

```text
agent_main
  -> ModelProviderAdapter
  -> PromptAssembler
  -> AgentService
  -> ToolBroker
  -> SQLite Store
```

最小原型：

```text
FakeModel
FakeTool
SQLite
AgentService
```

必须能够在完全没有 Codex、Hook 和 MCP Server 的情况下完成：

```text
Goal
-> PlanRevision
-> Task
-> Tool Invocation
-> Observation
-> Fact/Evidence
-> Review
-> TerminalJudge
```

第二阶段再加入 OpenAI-compatible Provider 和本地工具执行器。

---

## 13. AGENTS.md 定位

独立 Agent 的运行权威是 Prompt Compiler 和 Runtime，不是 AGENTS。

Codex Adapter 继续生成一个轻量项目 AGENTS：

- 沟通格式。
- Core/Adapter 依赖边界。
- 代码大小规则。
- 测试和发布命令。
- Prompt 修改顺序。
- 禁止第二状态机、技能库、RAG 和固定领域 Agent。

独立 Agent 本身使用 `PromptManifest + AgentConfig`，即使不存在 AGENTS 也能运行。

---

## 14. 从当前项目迁移

| 当前文件 | 独立 Agent 目标 |
|---|---|
| `codex/runtime/models.py` | `core/domain/*` |
| `codex/runtime/operation_runtime.py` | `core/application/service.py`、`scheduler.py`、`executor.py` |
| `codex/runtime/adaptive_planner.py` | `core/application/replanner.py` |
| `codex/runtime/verifier.py` | `core/application/verifier.py` |
| `codex/runtime/terminal_judge.py` | `core/application/terminal.py` |
| `codex/runtime/durable_store.py` | `adapters/storage/sqlite.py` |
| `codex/runtime/tool_broker.py` | Core selector + tool adapters |
| `codex/runtime/mcp_server.py` | `adapters/transports/mcp_server.py` |
| `codex/runtime/session_bridge.py` | `adapters/hosts/codex/session_binding.py` |
| `codex/hooks/core/intent_engine.py` | Core IntentClassifier |
| `codex/hooks/core/prompt_parser.py` | Codex Hook parser |
| `codex/launcher.py` | Codex Host launcher |
| `scripts/install.py` | Codex Host installer |
| `instruction.ctf.md` | Prompt Core fragments |
| `codex/prompts/*` | Prompt model profiles |
| `codex/AGENTS.md` | Codex compatibility artifact |

迁移期间原项目保留兼容导入和 MCP 工具名。

---

## 15. 实施阶段

### 阶段 0：基线冻结

- 保存当前 173 项测试结果。
- 保存 Prompt 快照和系统文件 Hash。
- 固化 MCP 输入输出 schema。

### 阶段 1：Core Contracts

- Goal、Plan、Fact、Evidence、Event、Review、Batch、Budget、Message 类型。
- Model/Tool/Store/Event/Host Ports 与 ProviderCapabilities。
- 保持现有行为不变。

### 阶段 2：AgentService

- 将 OperationRuntime 拆成 Service、BatchService、Scheduler、Executor。
- 建立 Provider-agnostic ModelLoop 和 ConversationManager。
- MCP Server 改为 Transport Adapter。
- SQLite 改为 Store Adapter。

### 阶段 3：Durability

- PlanRevision、Fact invalidation、Review。
- Attempt、Transcript、Snapshot、CAS、fencing、uncertain/reconcile、atomic commit。
- action/token/time budget、`paused_budget`、cancel/resume 和 Host Handoff。

### 阶段 4：Prompt Compiler

- PromptManifest、PromptBundle、Hash。
- 完整吸收 codex1 行为契约。
- 重写模型 Profile 和 Priming History。

### 阶段 5：Codex Adapter

- Hook 去状态机化。
- CLI/App 单 Profile。
- Runtime MCP 兼容。
- 安装器事务和配置保护。

### 阶段 6：Standalone Prototype

- FakeModel/FakeTool。
- `agent_main.py`。
- Provider system message 原生加载。
- 直接调用 AgentService 和 ModelLoop。
- 验证完整 Tool Call roundtrip、Transcript、预算暂停/恢复和取消传播。

### 阶段 7：Provider 与本地工具

- OpenAI-compatible Model Adapter。
- Provider capability negotiation、streaming 和 usage accounting。
- MCP Client Adapter。
- Local Executor Adapter。
- ExecutionPolicy。

### 阶段 8：最终收敛

- 重写 Codex AGENTS。
- 删除兼容 Wrapper。
- 补齐文档、升级和回滚策略。

---

## 16. 审查与验证

### 第一轮：结构审查

- Core 无 Codex/MCP/WebSocket 泄漏。
- Adapter 不反向控制 Domain。
- 状态所有权唯一。
- 单文件 200–400 行，硬上限 800。
- Prompt Core 无平台函数名。

### 第二轮：对抗审查

- 各事务点崩溃。
- 过期 Lease/fencing。
- 重复工具副作用。
- 工具输出 Prompt injection。
- 伪造 Observation。
- 跨目标 Evidence 污染。
- Fact 失效和 Fork 隔离。
- Priming History 被误当作事实。
- Multi-target 子 Run/批次聚合状态串扰。
- `paused_budget` 恢复导致重复副作用。
- Host Handoff 重放、重复领取和伪造回灌。
- cancel 与完成提交并发竞争。

### 第三轮：ModelLoop 与 Provider 审查

- `system/user/assistant/tool` Transcript 顺序和 Hash 稳定。
- 原生/模拟 Tool Call roundtrip 语义一致。
- 上下文压缩保留 Goal clauses、Plan 和 Evidence lineage。
- action/token/time budget 计量、暂停和恢复正确。
- streaming 中断、Provider 超时、取消和重试不产生伪 Evidence。
- 不同 ProviderCapabilities 组合均有确定性降级路径。

### 第四轮：Prompt Bank

每模型五项：

1. 完整复合意图。
2. 真实工具和 Artifact。
3. 自动失败恢复。
4. Evidence lineage。
5. 无用户搬运续航。

指标：

```text
Intent Clause Retention
Tool Execution Rate
Artifact Verification Rate
No-Relay Continuation Rate
Evidence Integrity
Terminal Accuracy
```

### 第五轮：Adapter 一致性

同一 Goal 通过：

```text
Direct Python API
Codex Runtime MCP
Codex CLI
Codex App
Standalone Agent
```

应产生相同的 GoalContract、PlanRevision、Evidence predicates 和 Terminal 语义。

### 第六轮：发布审查

- 独立虚拟环境安装。
- 无 Codex 环境运行。
- Codex Adapter 临时目录安装。
- Prompt Profile 匹配。
- 升级、卸载、数据迁移和回滚。

---

## 17. 验收标准

```text
无 Codex、Hook、MCP Server 时内核可完整运行
Codex 仅作为 Host/Transport Adapter
Prompt Core 与宿主无关
每个 Session 只有一个模型 Profile
codex1 行为契约全部进入 System Core/Intent Compiler
SQLite 是唯一权威状态
Event Log 可重建派生 Snapshot
计划修改具有 Revision/Fork 血缘
Fact 支持版本和失效传播
崩溃后通过 reconcile 避免盲目重复副作用
Multi-target batch 每目标隔离且聚合终态可验证
action/token/time budget 可计量，paused_budget 可持久恢复
Host Handoff 无用户搬运且支持防重放
cancel/resume 传播到 Provider、Tool 和批次子 Run，并保留清理证明
Operation CAS、Lease fencing 和 atomic commit 阻止过期/重复提交
ModelLoop 在原生/模拟 Tool Call 下具有一致 roundtrip 语义
Conversation 压缩可追溯且不丢失 Goal/Plan/Evidence 关键上下文
Provider capability negotiation、streaming、usage accounting 验证通过
无技能库、RAG、知识库和固定领域 Agent
Core 只依赖 Python 标准库
```

---

## 18. 优点与代价

### 优点

- 一次性解决 Codex 平台耦合。
- Prompt、Runtime 和工具协议可以跨宿主复用。
- 后续增加本地模型、API Provider、GUI 或远程 Worker 不再改核心。
- Codex 与独立 Agent 可以做行为一致性回归。
- 长期架构最清晰。

### 代价

- 初始重构范围明显大于轻量配置路线。
- 需要兼容层和数据迁移。
- Provider、独立模型循环和 Local Executor 都需要新增测试。
- 发布周期更长，适合作为新主版本或独立仓库推进。
