# Trace

Trace 是证据驱动、可持久恢复、面向强模型战术能力的专业红队 Agent Harness。

证据驱动、可持久恢复、面向强模型战术能力的专业红队 Agent Harness。

## 当前能力

- 版本化 Goal/Core Contracts 与 Provider/Tool/Worker Ports；
- `AgentService` 统一生命周期和 `OperationRuntime` 兼容层；
- Provider-agnostic ModelLoop、原生工具调用、并行、流式和恢复；
- 完整 Transcript、可追溯压缩和 action/token/time 预算；
- SQLite WAL、CAS、Lease/Fencing、幂等和取消竞争处理；
- Local/MCP/Codex/Docker Worker 边界和隔离 workspace；
- SHA-256 Artifact Store、FTS5、完整原始输出和有界模型投影；
- Phase 6 薄战术循环、append-only ExplorationLedger、分支重开和 ReconDigest；
- ToolRegistry 按能力/风险选择工具，支持目录 revision、按需 expand 和运行级可见性；
- 内置开源工具面：HTTP、DNS、TCP 探测、Playwright 浏览器、Capstone 反汇编、
  二进制信息/字符串、radare2/Rizin、Frida、源码搜索、Python AST 和云账号只读清单；
- 通用 run-scoped MCP Capability Plane，支持 roots、取消、目录刷新和资源清理；
- EvidenceGraph、SemanticVerifier、TerminalJudge 和五个公开 MCP 工具。

模型负责假设生成、工具选择、局部搜索和战术优先级；Runtime 只负责确定性不变量、证据晋升、清理和终态裁决。

## 本地运行

```powershell
python -m redteam_agent self-test
python -m redteam_agent.runtime.mcp_transport --root .\state
redteam-agent mcp-doctor --config .\config.toml
```

默认 `OperationRuntime` 直接注册上述工具，不依赖 `config.toml`。Playwright 和 Capstone
通过 Python API 调用，radare2/Rizin、Frida 和云 CLI 在本机存在时由内置 Adapter 直接
执行。`config.toml.example` 仅保留通用 MCP 扩展示例。

开发测试：

```powershell
python -m pytest -q
```

权威阶段路线见 `INDEPENDENT_AGENT_EVOLUTION_PLAN.md`；透明、轻量化重构的完整方案见
`docs/architecture/lean-transparent-refactor.md`；阶段验收见 `docs/acceptance/`。

重构明确参考 Pi coding-agent 的 session tree、selected tools、增量输出截断和
compaction boundary，但保留本项目的 SQLite/CAS、Lease、EvidenceGate 和 TerminalJudge
作为唯一权威。不把 JSONL、扩展或摘要当作事实源。
