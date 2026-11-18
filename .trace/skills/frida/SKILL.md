---
name: frida
description: 通过 Trace 已安装的 Python SDK 连接本地运行进程，使用 Frida 执行有界 JavaScript 插桩。 Inspect local running processes with Frida and execute bounded JavaScript instrumentation through the installed Trace Python SDK preset.
---

Discover the local tool `frida` using `tool_search`, then activate its stable ID with `tool_select`. The preset appears when Trace's selected Python imports `frida`; install it into that environment with `python -m pip install frida` when needed.

```json
{"action":"devices"}
{"action":"processes","device_id":"local"}
{"action":"instrument","pid":1234,"script":"send({pid:Process.id,modules:Process.enumerateModules().map(m=>m.name)});","duration_ms":500}
```

`instrument` accepts an existing `pid` or `process` name. `script` is ordinary Frida JavaScript, including Interceptor, memory inspection, and `send`. `duration_ms` controls the observation window. Script messages retain their native fields; binary message data is hex in `data`.

Each call unloads its script and detaches before returning, leaving the target process running. A JavaScript error or cleanup failure returns `ok:false` with a nonzero exit code. Use `messages`, `errors`, and `detached` together when interpreting results. Cancellation kills the worker and marks its outcome uncertain; SDK connection closure unloads its scripts, but arbitrary target-side changes made by the script are not rolled back. Sessions are not persistent and are not replayed on resume. This preset does not spawn processes or manage frida-server; `device_id` selects an already discovered device.
