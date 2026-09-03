# Phase 6.2 Acceptance - IDA Free Feasibility

## Decision

IDA Free is **not an executable backend for this project**. The supplied file is
an installer, not an analysis runtime:

```text
Path: C:\Users\Lin\Downloads\ida-free-pc_94_x64win.exe
Product: IDA Free 9.4
OriginalFilename: setup.exe
SHA-256: 95250D9503A3D5CEEE160F6174E44BBA28A12BA3A6B8DFB12D51F85C85AD5F83
```

The installer requested elevation on this host and was not installed.

## Compatibility finding

Hex-Rays' current IDA Free feature matrix excludes the IDAPython API and C++
SDK. It also does not support the IDA Pro plugin surface used by the inspected
IDA MCP project. Therefore the planned `-A -S` IDAPython bridge was tested and
discarded: it would expose a plausible schema but fail at runtime and could
produce false capability status.

The authoritative integration remains:

```text
IDA Pro + idalib-mcp --stdio
```

This preserves explicit `database` sessions, supervisor/worker lifecycle,
cursor pagination, cancellation and verified Artifact/Evidence lineage.

## Test evidence

```text
installer metadata inspection: pass
runtime executable discovery: no ida64.exe/idat64.exe found
IDA Free IDAPython/plugin compatibility: unsupported by vendor feature matrix
ida_free bridge implementation: removed before release
```

The runtime no longer exposes an `ida_free` preset or bridge. Configuring the
downloaded installer as an MCP command is treated as a failed discovery rather
than a connected server.

## Current project status

```text
pytest: 280 passed, 1 skipped
compileall: passed
```

The next valid IDA acceptance requires IDA Pro 8.3+ (9.x recommended), an
activated `idalib`, `uv`, and one real `idb_open -> analysis -> idb_close` run.
