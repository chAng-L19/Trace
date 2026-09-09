# Phase 6.1 Acceptance - Open-Source Capability Plane

## Result

L4 now has two complementary paths:

1. Built-in open-source adapters registered by `OperationRuntime` with no
   external configuration.
2. Generic run-scoped MCP adapters kept as optional extensions.

The model still proposes actions. Tool output first becomes an
Observation/Artifact and requires the existing verifier before Evidence
promotion.

## Built-in tools

| Area | Built-in adapters | Implementation |
|---|---|---|
| Web/API | `http-request`, `browser-navigate`, `browser-snapshot`, `browser-click`, `browser-fill`, `browser-screenshot` | Python standard library and Microsoft Playwright Python API |
| Network | `dns-resolve`, `port-probe` | Python `socket` |
| Reverse | `binary-info`, `binary-strings`, `binary-disassemble`, `binary-radare2`, `frida-processes` | PE/ELF/Mach-O/WASM parser, Capstone, radare2/Rizin and Frida when installed |
| Code audit | `code-search`, `python-ast-audit` | `pathlib`, `re`, Python `ast` |
| Cloud | `cloud-inventory` | Read-only locally installed AWS/Azure/GCP CLI invocation |

`binary-radare2`, `frida-processes` and `cloud-inventory` report a bounded
availability result when their optional local executable/package is absent;
they never cause an implicit download or execute a shell command.

## Invariants

- Built-in descriptors use `source = registered-adapter` and are available in
  the default broker without a config file.
- Side-effecting browser actions are explicitly annotated.
- HTTP, browser, network, binary and source paths are validated before use.
- Binary and tool output is bounded before it reaches model context.
- Full observation lineage remains available through the existing Artifact CAS.
- Optional MCP servers remain generic and run-scoped; no vendor-specific
  reverse-engineering lifecycle is embedded in the runtime.
- Tool output cannot mutate a descriptor, EvidenceGraph or TerminalDecision.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| Built-in descriptors register without config | `tests/test_open_source_tools.py::test_builtin_open_source_catalog` | pass |
| HTTP request returns bounded status/body/hash | `tests/test_open_source_tools.py::test_http_request_fixture` | pass |
| Browser adapter controls a local page | `tests/test_open_source_tools.py::test_browser_snapshot_and_click_fixture` | pass |
| Binary metadata, strings and Capstone disassembly work | `tests/test_open_source_tools.py::test_binary_tools_fixture` | pass |
| Source search and AST audit preserve file/line provenance | `tests/test_open_source_tools.py::test_code_audit_tools_fixture` | pass |
| Vendor-specific reverse lifecycle is absent | `rg` over `src`, `tests`, active config | pass |
| Existing MCP lifecycle remains compatible | `tests/test_mcp_capability_integrations.py` | pass |

## Runtime dependencies

```text
capstone 5.x
playwright 1.62.x
```

Playwright uses an installed Chrome/Chromium executable. radare2/Rizin,
Frida and cloud CLIs are optional local enhancements exposed through the same
built-in broker interface.
