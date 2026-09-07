# Lean L1 Acceptance - Canonical AgentService Entry

## Result

L1 makes `AgentService` the canonical application lifecycle entry without
changing the durable `OperationRuntime` internals. Public CLI and MCP operations
now enter through `AgentService`; `OperationRuntimeAdapter` remains a forwarding
compatibility shim for one migration cycle.

## Entry paths

```text
CLI self-test -> AgentService.start/run/summary
MCP transport -> RuntimeMcpServer(service=AgentService)
MCP redteam_run/status/cancel/observation -> AgentService
OperationRuntimeAdapter -> AgentService compatibility shim
AgentService -> OperationRuntime invariant implementation
```

Fake runtimes used by MCP contract tests retain a direct fallback. The production
constructor always creates or receives `AgentService`; the fallback is not a
second production orchestration path.

## Boundary changes

- `RuntimeMcpServer` owns a canonical `service` reference.
- MCP start/resume/status/cancel, budget, credential binding, target binding and
  observation submission route through service methods on the production path.
- `AgentService` exposes narrow compatibility operations for summary, credential
  binding, target binding and budget deltas.
- CLI self-test no longer imports or instantiates `OperationRuntime` directly.
- `OperationRuntimeAdapter` lifecycle methods forward to `AgentService`; its
  Store/Event/Tool ports remain until L2/L4 consolidation.
- MCP output continues using the historical `OperationResult.summary()` shape,
  preserving the five public tool schemas and current callers.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| MCP public run enters AgentService | `test_mcp_public_run_routes_through_agent_service` | pass |
| RuntimeMcpServer creates canonical service | `test_runtime_mcp_server_constructs_canonical_service` | pass |
| Legacy adapter is a forwarding shim | `test_legacy_runtime_adapter_is_an_agent_service_shim` | pass |
| Existing adapter contracts remain equivalent | `tests/test_runtime_core_adapter.py` | pass |
| MCP handoff, receipt and restart semantics remain | `tests/test_mcp_handoff.py` | pass |
| Secret redaction remains | `tests/test_persistence_redaction.py` | pass |
| CAS/Lease/cancellation races remain | complete regression | pass |
| MCP five-tool public schema remains | Phase 0 snapshot and live initialize/tools/list | pass |

## Validation record

```text
targeted L1 and compatibility tests: 47 passed
complete regression: 285 passed, 1 skipped
compileall: passed
Phase 0-6 snapshots: verified
CLI installed-path self-test: completed, terminal success
MCP initialize: redteam-agent-runtime / 2025-06-18
MCP tools/list: 5 public tools
wheel SHA-256: 265C1A00CDCD2EF9A45E902C7C846D16459944C045D04F2DF368E943A146896C
isolated wheel install: passed
```

## Complexity record

```text
production Python files: 99
production Python lines: 23,918
largest file: src/redteam_agent/runtime/mcp_server.py (800 lines)
files over 800 lines: 0
```

L1 temporarily adds routing code before L2/L3 remove duplicate mapping and loop
paths. No new manager, persistence layer or dependency was introduced.

## Remaining shim

`OperationRuntimeAdapter`, `RuntimeStoreAdapter`, `RuntimeEventAdapter` and
`RuntimeToolAdapter` remain exported for current callers. L2/L4 must migrate their
remaining port tests and delete the redundant mapping or retain only a logic-free
re-export shim. New production code must use `AgentService` directly.
