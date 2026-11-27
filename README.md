# Trace

Trace 是一个支持 CLI 和 WebUI 的通用 AI Agent，用于研究分析、工具执行和多步骤任务处理。

- **任务管理**：保存运行记录，支持暂停、恢复和补充指令。
- **Worker 协作**：拆分任务并协调多个 Worker。
- **工具扩展**：接入模型服务、MCP、Skills 和浏览器工具。
- **过程追踪**：查看实时对话、探索链路、执行证据和报告。

## 启动

需要 Go 1.22+；模型—工具循环为内置 Go 实现。Node.js 22.19+ 仅供 web-analysis 静态分析工具使用。以下以 PowerShell 为例：

```powershell
go build -buildvcs=false -o trace.exe ./cmd/trace
.\trace.exe serve --addr 127.0.0.1:8765
```

浏览器访问 `http://127.0.0.1:8765`，在界面中配置模型服务并创建任务。

## 角色提示词

新 Run 按职责固定加载提示词：`prompts/agent.md` 保存共用规则，协调者追加 `coordinator.md`，执行 Worker 追加 `worker.md`，`execution_role="recon"` 的采集 Worker 再追加 `recon.md`。Recon 的提示词选择独立于模型配置，未单独配置 Recon 模型时仍使用采集角色提示词。恢复及分支沿用已保存的提示词和哈希，不回写历史 Run。

默认提示词随二进制嵌入；部署目录中的角色文件也应成套保留。`TRACE_PROMPT_FILE` 指向开头带 `<!-- trace:role-prompts:v1 -->` 标记的共用文件时，从同目录读取三份角色文件，缺失或空文件会报错。没有该标记的自定义单文件保持原行为，各角色使用该文件的完整原文。 CLI 恢复已有 Run 不依赖当前模板文件；后续派发新 Worker 时会读取配置路径，文件缺失会阻止新建，不会静默换成默认提示词。

## 协作与完成验收

有限目标可以在计划任务的 `work_items` 声明对象身份与成功条件；覆盖判定要求它们匹配同一条原始观察。阶段完成后发现的剩余工作保存在 `trace_result.remaining`，后继任务通过 `continues` 关联，重启后继续保留。主控自己的剩余声明使用 `continues: [{"task_id":"$run","item_id":"声明ID"}]`，无需为保留来源标识 `$run` 创建任务或依赖。声明结构错误会阻止成功，已知 ID 可在后续声明补齐字段；无 ID 或数组结构错误会保留诊断，后续须显式完整重述剩余数组并覆盖全部已知义务才能解除。探索图默认展示任务主线与范围覆盖，证据和历史仍可展开、检索。开放研究无需预先枚举全部目标。

创建 Run 的 API 可显式提供 `resource_limits.worker_ceiling`，字段为 `capacity`（1–32）、`source`（约束来源）和 `scope: "root-tree"`。该限制约束整棵任务树的 Worker 槽位；外部资源操作仍由任务的 `resource_keys` 协调。Worker 请求会分别显示耗时估算与真实预算，未配置预算时不会生成倒计时。

执行中若 Worker 访次或来源变化，原 Action 保持未知，先核对外部效果。现有工具核对 API 的 `content` 可显式提交 `{"executor_ownership_review":{"changed_event_sequence":123,"effects_reconciled":true,"observation":"实际核对经过与结论","evidence_ids":[]}}`，并在外层填写原 `request_id` / `call_id`；序号必须对应该 Action 的 `action_executor_changed`。这只解除经人工核对的归属占用，不把原始未知结果变成成功，也不替代真实进程清理或重放请求。
