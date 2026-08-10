# Agent 首批迁移状态

## 来源

```text
E:\cli\codex-redteam-mode\codex-redteam-mode-main\codex\runtime
E:\cli\codex-redteam-mode\codex-redteam-mode-main\codex\workflows
```

## 当前落点

```text
src/redteam_agent/runtime
src/redteam_agent/workflows
tests
```

## 已完成

- 迁入 44 个 Runtime Python 模块。
- 迁入 `generic-adaptive.toml` 与 `profiles.toml`。
- 建立独立 `pyproject.toml`、CLI 与 MCP stdio 入口。
- 默认状态根切换为 `REDTEAM_AGENT_HOME` 或 `~/.redteam-agent`。
- Codex Session Bridge 改为仅在显式设置 `CODEX_HOME` 时启用。
- MCP 身份改为 `redteam-agent-runtime`，并保留五个公开控制面工具。
- 迁入并适配 Runtime、证据、持久化、Handoff、规划和终态测试。

## 当前阶段边界

- Codex 项目源文件尚未删除或改写。
- 当前 Agent 保留迁移前 Runtime 行为，后续再进行 Ports/Adapters 目录重构。
- Codex Hook、系统 Profile、历史注入和 Host 红队模式仍留在 Codex 项目。

## 独立验证

```text
compileall: passed
pytest: 142 passed
wheel: codex_redteam_agent-0.1.0-py3-none-any.whl
wheel sha256: c1d2e19228c89a90ffc9a39d97c38d67dde33c4227a79e3e0414439bf9323e96
isolated install: passed
self-test: completed / terminal success
MCP initialize: redteam-agent-runtime / protocol 2025-06-18
MCP tools: 5
```

最终隔离安装目录：

```text
E:\cli\.tmp\redteam-agent-install-final-f20742c269604952a428af750c02c507
```
