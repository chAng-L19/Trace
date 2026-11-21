# Trace

Trace 是一个支持 CLI 和 WebUI 的通用 AI Agent，用于研究分析、工具执行和多步骤任务处理。

- **任务管理**：保存运行记录，支持暂停、恢复和补充指令。
- **Worker 协作**：拆分任务并协调多个 Worker。
- **工具扩展**：接入模型服务、MCP、Skills 和浏览器工具。
- **过程追踪**：查看实时对话、探索链路、执行证据和报告。

## 启动

需要 Go 1.22+、Node.js 22.19+。以下以 PowerShell 为例：

```powershell
npm ci --prefix internal/picore --omit=dev --ignore-scripts
$env:TRACE_PI_RUNTIME = (Resolve-Path internal/picore).Path
go build -buildvcs=false -o trace.exe ./cmd/trace
.\trace.exe serve --addr 127.0.0.1:8765
```

浏览器访问 `http://127.0.0.1:8765`，在界面中配置模型服务并创建任务。
