---
name: web-analysis
description: 按任务选择网页交互、浏览器被动证据、已保存流量或本地 JavaScript 分析工具；传递真实采集与清单身份，不运行固定全量扫描。 Choose existing Trace tools for webpage interaction, passive browser evidence, saved network captures or local JavaScript analysis; hand off actual capture and manifest identities without running a fixed full scan.
---

# Choose the smallest relevant path

Use this skill when a task needs web/JavaScript evidence or an existing browser capture must become a local analysis input. It is navigation over installed tools, not another scanner or execution loop.

1. Search the capability you need with `tool_search`, inspect the returned metadata, then `tool_select` its actual stable ID. Read the selected tool's current schema in the next request before constructing arguments. Names below are search terms, never invented IDs or a substitute for the schema. An empty result or missing dependency is an observed limitation; do not claim the capability ran.
2. Choose one entry based on available input:
   - **Interact with a webpage:** prefer `lightpanda`, read its skill, and keep navigation, login, DOM checks and effect verification in one bounded call. Use backup `cloakbrowser` when Lightpanda is unavailable/unsupported or screenshots and actual rendering are needed. Each call starts a fresh private context; login does not survive another call. Inspect prior effects before changing browsers; never automatically replay an uncertain submission. For a recorded action sequence search `capture_browser_scenario` and use its returned schema.
   - **Observe a known browser target:** search `list_browser_targets`, then use an actual returned target with `inspect_web_page`, `observe_web_session` or `analyze_web_bundle` only as needed. CDP endpoints/target IDs must come from the task or observed output; do not guess an operator browser. `discover_webmcp_tools` discovers declarations, not invocation or proof.
   - **Inspect a saved network capture:** search `inspect_web_network_capture`; pass the real HAR/SAZ/mitmproxy path and supported format, preserving record order and missing bodies.
   - **Analyze local JavaScript/ASAR:** search `analyze_javascript_application`; pass the real input path. Query a produced graph with `trace_javascript_semantics` or `trace_application_feature`. Runtime reconciliation requires separately observed runtime evidence, not an inferred execution.
3. Stop once the task has enough evidence. Do not launch all operations, fetch every source map, recover all scripts, or start an app merely because the tools exist. Respect the existing request/tool/tree budgets and cancellation.

# Hand off actual artifacts

- Persist a successful page/scenario capture in the Run workspace before exporting. Use the capture's actual absolute path for `export_web_scripts.capture_path`; choose an absent `output_directory` whose parent exists. Never reconstruct script bytes from preview text.
- Inspect the returned `capture_sha256`, `capture_completeness`, `limitations`, each script's `content.state`, and `manifest.path/sha256/bytes`. Retain unavailable/partial/unknown entries; a manifest is not proof that all scripts or original sources were captured.
- Only when the result supplies non-null `analysis_input`, pass its actual `input_path` to `analyze_javascript_application`. `trace_web_module_imports` and `trace_web_source_location` consume identities/paths from actual export output, using their current selected schemas. Do not fabricate a manifest, module or source-map path.
- `analyze_web_bundle.fetch_source_maps` is opt-in. A declared URL is not a fetched map; enable fetching only when needed, within existing network/origin/byte limits. Record failed, filtered and partial maps explicitly.
- Register real deliverables through `artifact_register` using returned filesystem paths; preserve original-byte hash and the producing action/request identity. `artifact_read` is a bounded content inspection, not a replacement for original bytes or execution verification.
- A timed-out submission has uncertain effects: inspect actual state before deciding on another action. Cleanup completion does not prove rollback. Tool completion, task success and verified source evidence remain separate. Use the existing finding/method/report rules rather than introducing another report format.
