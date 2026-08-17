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
- EvidenceGraph、SemanticVerifier、TerminalJudge 和五个公开 MCP 工具。

模型负责假设生成、工具选择、局部搜索和战术优先级；Runtime 只负责确定性不变量、证据晋升、清理和终态裁决。

## 本地运行

```powershell
python -m redteam_agent self-test
python -m redteam_agent mcp -- --root .\state
```

开发测试：

```powershell
python -m pytest -q
```

权威阶段路线见 `INDEPENDENT_AGENT_EVOLUTION_PLAN.md`，阶段验收见 `docs/acceptance/`。

