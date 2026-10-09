# Trace

Trace 是证据驱动、可持久恢复、面向强模型战术能力的专业红队 Agent Harness。

## 当前能力

- 版本化 Goal/Core Contracts 与 Provider/Tool/Worker Ports；
- `AgentService` 统一生命周期和 `OperationRuntime` 兼容层；
- Provider-agnostic ModelLoop、原生工具调用、并行、流式和恢复；
- 完整 Transcript、可追溯压缩和 action/token/time 预算；
- SQLite WAL、CAS、Lease/Fencing、幂等和取消竞争处理；
- Local/MCP/Docker Worker 执行与隔离 workspace，Codex 持久交接与结果回填；
- SHA-256 Artifact Store、FTS5、完整原始输出和有界模型投影；
- Phase 6 薄战术循环、append-only ExplorationLedger、分支重开和 ReconDigest；
- ToolRegistry 按能力/风险选择工具，支持目录 revision、按需 expand 和运行级可见性；
- 内置开源工具面：HTTP、DNS、TCP 探测、Playwright 浏览器、Capstone 反汇编、
  二进制信息/字符串、原生惰性二进制查询、Frida、源码搜索、Python AST、云账号只读清单，
  云区域/资源枚举，以及 ASC 思路的 APK/DEX 按需分析、跨 DEX 引用查询和加固迹象探针；
- 可选 Sliver C2 RPC：健康检查、Session/Beacon/Listener 清单与显式 Session 命令执行；
- 通用 run-scoped MCP Capability Plane，支持 roots、取消、目录刷新和资源清理；
- 统一 `agent:prepare_tools` 按需准备依赖、更新工具可用性，公开 MCP 与内置工具分别展示；
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
Provider 支持 `openai-compatible` 与原生 `anthropic`，使用 `--provider`、`--model`、
`--api-base-url`、`--api-key-env` 或对应 `TRACE_*` 环境配置。CLI 密钥通过环境变量传入。

```powershell
trace start "审计本地项目" --target .\project --provider anthropic --model YOUR_CLAUDE_MODEL --max-output-tokens 8192
```

原生 Claude 使用 Messages API，默认读取 `ANTHROPIC_API_KEY`；支持工具调用、流式响应、
签名续接与推理配置。手动 thinking 必须设置 `--max-output-tokens` 大于
`--thinking-budget-tokens`；网关需支持 `thinking.display=omitted`，明文 thinking 响应会被拒收。
OpenAI Chat/Responses 的显式 refusal 会保留，有限恢复不会重试拒绝、认证或协议错误。
支持持久 Provider 配置、跨 worker 执行历史检索、证据版本与按需 Skill。
本地任务验收覆盖执行纠偏、原始采集、上下文保留、恢复和证据完整性；不等于真实模型验收。

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

基础发行包只安装 Runtime，工具依赖按需准备。可使用 `trace setup TOOL`，或由模型调用
`agent:prepare_tools`；一次准备返回安装结果、可用工具和新的目录版本。需要完整常用工具包时
可安装 `trace-agent[tools]`。未安装的依赖会标记为不可用，原生替代能力继续保留。

默认 `OperationRuntime` 直接注册上述工具，不依赖 `config.toml`。Playwright、Capstone
和 ASC 风格 APK/DEX 查询通过 Python API 调用；radare2/Rizin、Frida 和云 CLI/SDK 在本机存在时
由内置 Adapter 使用，缺少 radare2/Rizin 时自动回退到原生惰性二进制查询。
`config.toml.example` 提供可选 Provider 配置与通用 MCP 扩展示例。
Web MCP 页面统一展示网页托管与配置文件中的外部服务；官方 Playwright MCP 未配置时显示
待接入并预填官方启动参数，不将内置 Python 浏览器适配器当作 MCP 连接。

Docker worker 使用任务指定的 `image` 与 `argv`，共享运行工作区并保存 stdout/stderr 产物；
支持非零退出、超时、取消、进程中断恢复和容器清理。需要本机 Docker CLI 与可用 daemon。
任务容器默认非 root、只读根目录、无 capabilities、禁止提权、网络 `none`；工作区和
64 MiB `/tmp` 可写。部署端 CPU/内存/PID 上限默认为 1/512 MiB/128，可通过
`TRACE_DOCKER_MAX_CPUS`、`TRACE_DOCKER_MAX_MEMORY_MB`、`TRACE_DOCKER_MAX_PIDS` 调整；
任务只能降低限额，联网任务可指定 `network=bridge`。Linux 宿主需非 root 运行，
工作区路径和 UID/GID 必须与 Docker daemon 所在主机一致。
Codex worker 仍是持久交接接口，外部宿主负责接收任务和回填结果。

Web 强制用户名密码登录，首次启动创建管理员 `trace / admin@123`；在个人资料中可修改
用户名、显示名与密码，管理员可以添加成员或管理员。`TRACE_ADMIN_USERNAME/PASSWORD`
仅在用户库首次初始化时使用，重启不会覆盖已修改账户。修改账密会使其他旧会话失效。
全局 Provider、Skill、MCP 配置写入仅允许管理员；运行及其证据是可信成员共享的工作台，
不提供租户隔离。Web 会话保留在单个服务进程中，服务重启后需重新登录。

`cloud-inventory` 的 GCP/Azure 凭据验证会执行只读远程权限探针；本地 CLI
账号缓存不作为有效凭据证明。`verification_source` 表示验证来源，
`identity_verified` 区分远程身份验证与本地账号元数据。显式 GCP access token
不绑定本机 CLI 账号；显式 AWS/GCP 凭据会清除环境中的 endpoint/身份覆盖配置。
腾讯、阿里与华为、火山、百度、京东通过现有统一云入口接入。后四者按需安装官方 SDK，
目前支持计算资源的远程只读探针与清单；其他资源类型会明确返回不支持。

Sliver 集成按需安装，不进入默认依赖：

```powershell
python -m pip install "trace-agent[sliver]"
```

调用内置 `builtin:sliver-c2` 时，只传入环境变量名，不把 token、CA、客户端证书和私钥放进工具参数；支持
`health`、`sessions`、`beacons`、`listeners` 和显式 `execute`。执行动作要求 `session_id`、可执行路径与参数数组，
不接受 shell 字符串；RPC 响应、输出字节数和条目数均有上限。

发布门禁直接验证源码编译、前端 JavaScript、依赖一致性、自检、wheel 内容、
隔离安装、六个命令入口和 MCP 五工具的真实 stdio 调用闭环。CI 还检查实际 Docker worker
生命周期与四个官方云 SDK 的请求序列化；SDK 测试使用本地传输替身，不访问云账号。

重构明确参考 Pi coding-agent 的 session tree、selected tools、增量输出截断和
compaction boundary，但保留本项目的 SQLite/CAS、Lease、EvidenceGate 和 TerminalJudge
作为唯一权威。不把 JSONL、扩展或摘要当作事实源。
