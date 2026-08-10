# Codex Red-Team Agent

独立、宿主无关的持久化红队 Agent Runtime。当前首批迁移包含：

- Goal/Prompt Rewrite/Workflow 编译
- 单一 `generic-adaptive` 工作流和数据型 Profiles
- Scheduler/Executor/ToolBroker
- SQLite 状态、CAS、Lease/Fencing
- Fact/Evidence/Review/TerminalJudge
- MCP stdio 控制面

## 本地运行

```powershell
python -m redteam_agent self-test
python -m redteam_agent mcp -- --root .\state
```

开发测试：

```powershell
python -m pytest -q
```

当前阶段保留原 Runtime 行为，后续再将 Codex Session Bridge 移入独立 Host Adapter。

