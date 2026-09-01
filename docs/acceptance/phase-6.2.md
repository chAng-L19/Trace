# Phase 6.2 Acceptance - IDA Free Read-only Bridge

## Decision

The supplied file is an installer:

```text
C:\Users\Lin\Downloads\ida-free-pc_94_x64win.exe
Product: IDA Free 9.4
OriginalFilename: setup.exe
SHA-256: 95250D9503A3D5CEEE160F6174E44BBA28A12BA3A6B8DFB12D51F85C85AD5F83
```

It is not an executable IDA runtime and therefore is not passed to an MCP
server as if it were `ida64.exe`/`idat64.exe`. The installer requested elevation
on this host and was not installed automatically.

## Integration model

IDA Free does not expose `idalib` and the inspected IDA Pro MCP project rejects
IDA Free plugin installation. The project therefore adds an independent bridge:

```text
Agent ToolBroker (run-scoped MCP client)
  -> ida_free_bridge (MCP stdio server)
     -> ida64.exe/idat64.exe -A -S ida_free_agent.py target.bin
        -> loopback JSON command channel
```

The bridge starts one IDA process per explicit database session. The embedded
IDAPython endpoint exposes only read-oriented analysis operations. Every tool
call carries `database`; no implicit current database is used.

## Exposed read-only surface

```text
idb_open / idb_list / idb_close
server_health
list_funcs / imports
decompile / disasm / xrefs_to
get_string / get_bytes / get_int
```

`idb_open` and `idb_close` are lifecycle operations; analysis tools are marked
read-only in MCP annotations and the `ida_free` preset excludes rename/patch/
write APIs. Full tool results still pass through the existing Observation,
Artifact and Evidence boundaries.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| `ida_free` preset is run-scoped and bounded | `test_ida_free_preset_is_read_only_and_token_bounded` | pass |
| MCP initialize/tools/list contract | `test_ida_free_bridge_mcp_initialize_and_tools_list` | pass |
| Installer/runtime distinction | `test_ida_free_installer_path_is_rejected_without_launching` | pass |
| Invalid calls remain MCP errors | `test_ida_free_bridge_reports_invalid_tool_as_mcp_error` | pass |
| Actual IDA Free analysis session | requires installed `ida64.exe`/`idat64.exe` | pending host install |

## Operator configuration

After installing IDA Free, configure the installed runtime, not the downloaded
setup file:

```toml
[mcp_servers.ida_free]
preset = "ida_free"
scope = "run"
command = "{python}"
args = ["-m", "redteam_agent.runtime.ida_free_bridge", "--ida", "C:\\Program Files\\IDA Free 9.4\\idat64.exe"]
startup_timeout_seconds = 180
tool_timeout_seconds = 240
```

Then run:

```powershell
redteam-agent mcp-doctor --config .\config.toml
```

The expected status is `catalogued` with the bounded read-only tool count. The
first actual binary operation is `idb_open(input_path=...)`; subsequent calls
must use the returned `database` session ID.

## Regression record

```text
compileall: passed
pytest: 285 passed, 1 skipped
wheel: codex_redteam_agent-0.1.0-py3-none-any.whl
wheel SHA-256: 75C723F72D08DFE64DCE5DA6EFD8F027E1E9AC27E610E60CBA66898AD7F8591B
isolated install: passed
installed self-test: terminal=true, success=true
```
