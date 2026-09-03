# Phase 6.1 Acceptance - Stateful MCP Capability Plane

## Result

Phase 6.1 adds first-class, run-scoped MCP integrations for Microsoft Playwright
and IDA Pro without moving tactical authority into the runtime. It is a Phase 7
foundation increment: tool output remains an Observation/Artifact until an
existing verifier promotes it.

## Source references

| Reference | Adopted property |
|---|---|
| OpenCode `packages/opencode/src/mcp/index.ts` | explicit MCP lifecycle state, scoped transports, tools-list change handling |
| OpenCode `packages/opencode/src/tool/registry.ts` | bounded tool registry separate from server lifecycle |
| OpenCode `packages/opencode/src/session/compaction.ts` | original history remains authoritative after compaction |
| `E:\cli\cc_src\src\services\mcp` | config scopes, pending/failed state, timeout/cancellation and cache invalidation |
| `E:\cli\cc_src\src\QueryEngine.ts` | bounded tool output before micro/autocompaction |
| Microsoft Playwright MCP | accessibility snapshot/find, workspace roots, isolated context, read-only annotations |
| IDA Pro MCP | supervisor + worker sessions, explicit database argument, profile-based tool exposure |

Reference commits used during implementation:

```text
opencode        7774461bbf7bd0600070cdede4fe8b9d9f301bf4
playwright      c377b7f47b00d41f4639b99b66ef62a458529f46
playwright-mcp  7e0457a7cbf88823bf0146d12c46ae12c6818247
ida-pro-mcp     3349ae30c6eb7fa1c14b158ff71bfc7c3081bb51
```

## Invariants

- Playwright and IDA presets default to `scope = "run"`.
- Each run receives a distinct client, cwd and MCP roots entry.
- `run_id`, workspace and environment placeholders bind only when the run client starts.
- MCP tool responses enter ToolResult/Artifact boundaries; no tool or model writes Evidence directly.
- `tools/list_changed` invalidates and rebuilds that server's catalog.
- Cancellation maps the durable external call ID to the active JSON-RPC request ID.
- Terminal close tracks IDA sessions returned by this run's `idb_open` and calls
  `idb_close(database=..., save=true)` only for those sessions.
- Cleanup outcomes are persisted as runtime events, not promoted to cleanup Evidence.
- Duplicate process signatures are catalogued once.

## Token policy

Tool visibility is reduced only through reversible presets:

- Playwright: accessibility/navigation/forms/network/console/evaluate/screenshot tools.
- IDA: database management, inventory, decompile/disasm, xrefs, graph, data-flow,
  string/bytes/types/stack reads.
- `include_tools = ["*"]` restores the full remote catalog when a task needs it.
- Complete tool schemas remain available to the primary model; a compact per-server
  catalog is recorded as request metadata for observability.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| Preset filtering and side-effect annotations | `test_playwright_and_ida_presets_expose_high_value_bounded_catalog` | pass |
| Run client/workspace/root isolation | `test_run_scoped_mcp_clients_are_isolated_reused_and_closed` | pass |
| IDA owned-session cleanup | `test_ida_run_cleanup_only_closes_sessions_opened_by_that_run` | pass |
| Tools-list change refresh | `test_run_client_tool_change_notification_refreshes_catalog` | pass |
| Duplicate process suppression | `test_duplicate_mcp_process_signature_is_not_started_twice` | pass |
| Placeholder/environment binding | `test_mcp_spec_expands_environment_and_run_placeholders` | pass |
| Official Playwright initialize/tools-list | `redteam-agent mcp-doctor`; 17 bounded tools | pass |
| Live Playwright local page | `browser_navigate`, `browser_snapshot`, `browser_find` | pass |
| Playwright run cleanup | active clients `1 -> 0`; process/browser closed | pass |
| Real IDA connection | host lacks IDA Pro, idalib and `uv` | environment pending |
| IDA Free feasibility | official feature matrix: no IDAPython/API/plugin support | unsupported |

## Live Playwright proof

The official `@playwright/mcp@0.0.79` server was started through the production
`StdioMcpClient`. Against a deterministic localhost page:

```text
browser_navigate  success  title=Playwright Fixture
browser_snapshot  success  heading="Redteam MCP Fixture" button="Probe"
browser_find      success  matches=1
run clients       1 before close, 0 after close
```

The production example uses only options accepted by the inspected official CLI:

```text
--isolated --headless --image-responses=omit --codegen=none
--block-service-workers --output-dir={workspace}/playwright-output
```

## IDA environment record

The current host has no `uv`, IDA Pro installation, or activated `idalib-mcp`.
Therefore IDA acceptance is protocol/configuration level on this host. A real
IDA gate must run `mcp-doctor`, `idb_open`, one explicit-database analysis call,
and terminal `idb_close` on a host with IDA Pro 8.3+ (9.x recommended).

The host contains `C:\Users\Lin\Downloads\ida-free-pc_94_x64win.exe`; PE metadata
identifies it as **IDA Free 9.4 setup.exe**, not an analysis runtime. Hex-Rays'
current IDA Free feature matrix explicitly excludes the IDAPython API and C++
SDK, while the inspected IDA Pro MCP implementation explicitly rejects IDA Free
plugin installation. An MCP bridge based on IDAPython or that plugin therefore
does not satisfy executable acceptance and is not shipped.

## Changed files

```text
src/redteam_agent/runtime/mcp_config.py
src/redteam_agent/runtime/mcp_clients.py
src/redteam_agent/runtime/tool_broker.py
src/redteam_agent/adapters/runtime.py
src/redteam_agent/application/agent_service.py
src/redteam_agent/application/model_loop.py
src/redteam_agent/workers/mcp.py
src/redteam_agent/cli.py
config.toml.example
tests/test_mcp_capability_integrations.py
```
