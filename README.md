# Trace

Trace 是证据驱动、可持久恢复、面向强模型战术能力的专业红队 Agent Harness。

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
  二进制信息/字符串、原生惰性二进制查询、Frida、源码搜索、Python AST、云账号只读清单，
  以及 ASC 思路的 APK/DEX 按需分析、跨 DEX 引用查询和加固迹象探针；
- 通用 run-scoped MCP Capability Plane，支持 roots、取消、目录刷新和资源清理；
- EvidenceGraph、SemanticVerifier、TerminalJudge 和五个公开 MCP 工具。

模型负责假设生成、工具选择、局部搜索和战术优先级；Runtime 只负责确定性不变量、证据晋升、清理和终态裁决。

## 本地运行

```powershell
python -m pip install .
trace self-test
trace setup chromium rizin
trace doctor --json
trace-mcp --root .\state
trace mcp-doctor --config .\config.toml
trace-web --root .\state
```

CLI 生命周期默认输出 JSON；`events --jsonl` 输出逐条事件。状态根目录优先级为
`--root`、`TRACE_HOME`、`REDTEAM_AGENT_HOME/operations`（默认 `~/.redteam-agent/operations`）。
Provider 使用 `--model`、`--api-base-url`、`--api-key-env` 或对应 `TRACE_*` 环境配置，密钥只通过环境变量传入。

```powershell
trace start "审计本地项目" --target .\project --root .\state --max-actions 64
trace run RUN_ID --root .\state
trace status RUN_ID --root .\state
trace events RUN_ID --root .\state --jsonl
trace evidence RUN_ID --root .\state
trace resume RUN_ID --root .\state --add-actions 32
trace cancel RUN_ID --root .\state --reason operator_stop
# 先停止 Web、MCP 与 workers；恢复使用全新目录。
trace state backup .\state.zip --root .\state
trace state verify .\state.zip
trace state restore .\state.zip --root .\restored-state
```

备份包含完整状态目录、已合并 WAL 的 SQLite 快照、CAS、工作区、凭据密钥、托管配置和工具清单，
恢复前逐文件校验 SHA-256 与 schema。活动运行/租约、根目录进程锁和快照期间文件变化会使备份失败；
暂停或取消运行并停止服务后重试。归档含密钥，应按凭据文件保管；外置工具缓存与环境配置另行归档。
Linux、Docker、systemd 与升级回滚见 [部署说明](deploy/README.md)。

发行包名与主命令统一为 `trace-agent` / `trace`。Python 导入路径 `redteam_agent`
以及原有 `redteam-agent*` 命令继续作为兼容接口保留。

默认 `OperationRuntime` 直接注册上述工具，不依赖 `config.toml`。Playwright、Capstone
和 ASC 风格 APK/DEX 查询通过 Python API 调用；radare2/Rizin、Frida 和云 CLI 在本机存在时
由内置 Adapter 使用，缺少 radare2/Rizin 时自动回退到原生惰性二进制查询。
`config.toml.example` 仅保留通用 MCP 扩展示例。

发布门禁直接验证源码编译、前端 JavaScript、依赖一致性、自检、wheel 内容、
隔离安装、六个命令入口和 MCP 五工具协议。

重构明确参考 Pi coding-agent 的 session tree、selected tools、增量输出截断和
compaction boundary，但保留本项目的 SQLite/CAS、Lease、EvidenceGate 和 TerminalJudge
作为唯一权威。不把 JSONL、扩展或摘要当作事实源。
