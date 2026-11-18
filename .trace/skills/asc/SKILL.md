---
name: asc
description: 使用 Droid ASC 检查 Android APK：列出类、解析清单、查找引用并按需反编译，无需全量项目索引。 Inspect Android APKs with Droid ASC: list classes, decode manifests, find references, and decompile selected classes without a full-project index.
github_url: https://github.com/MG1937/ASC
local_path: /opt/trace/runtime/ASC
---

Use Trace's registered local tool `droidasc`. This is a local Python CLI, not an MCP server. Discover its stable ID with `tool_search`, then activate it with `tool_select` before calling it. Arguments are an `argv` array; each item is one literal argument, so do not add shell quoting around paths or descriptors.

```json
{"argv":["listclass","/path/app.apk","--prefix","com.example"]}
{"argv":["getmanifest","/path/app.apk"]}
{"argv":["findrefs","/path/app.apk","string","token"]}
{"argv":["findrefs","/path/app.apk","method","onCreate","--class","com.example.Main"]}
{"argv":["findrefs","/path/app.apk","field","apiKey"]}
{"argv":["getclass","/path/app.apk","com.example.Main"]}
```

Use absolute APK paths. Prefer `listclass` or `findrefs` to locate a class before requesting `getclass`. `getclass` accepts Java names and Dalvik descriptors. `--prefix` belongs to `listclass`; `--class` narrows method/field searches. Pass `--help` or a subcommand followed by `--help` for the installed version's full options. Default output is stdout; append `-o` and an output path when a source/XML/text file is requested. Do not pass `--gui` in a tool call.

The image contains the frozen `droidasc` Python module at `/opt/trace/runtime/ASC`; startup does not install from the network. Installation or discovery alone does not prove every decompilation path works on an arbitrary APK.
