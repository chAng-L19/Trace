# Codex Red-Team Agent

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
- run-scoped MCP Capability Plane，内置 Playwright preset、roots、取消、目录刷新和资源清理；
- EvidenceGraph、SemanticVerifier、TerminalJudge 和五个公开 MCP 工具。

模型负责假设生成、工具选择、局部搜索和战术优先级；Runtime 只负责确定性不变量、证据晋升、清理和终态裁决。

## 本地运行

```powershell
python -m redteam_agent self-test
python -m redteam_agent mcp -- --root .\state
redteam-agent mcp-doctor --config .\config.toml
```

`config.toml.example` 提供官方 Playwright MCP 配置。Playwright preset 默认保留完整交互
能力但优先暴露高价值工具；逆向后端通过用户自己的 MCP 配置或本地 Ghidra/radare2/
Frida 工具接入，不绑定 IDA 安装器或 GUI。

开发测试：

```powershell
python -m pytest -q
```

权威阶段路线见 `INDEPENDENT_AGENT_EVOLUTION_PLAN.md`；透明、轻量化重构的完整方案见
`docs/architecture/lean-transparent-refactor.md`；阶段验收见 `docs/acceptance/`。

重构明确参考 Pi coding-agent 的 session tree、selected tools、增量输出截断和
compaction boundary，但保留本项目的 SQLite/CAS、Lease、EvidenceGate 和 TerminalJudge
作为唯一权威。不把 JSONL、扩展或摘要当作事实源。
