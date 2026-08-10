# Phase 1 Acceptance - Core Contracts and Ports

## Result

Phase 1 is accepted. The project now has a standard-library-only Core contract
layer and explicit ports, while the existing `OperationRuntime` remains the
authoritative execution implementation behind compatibility adapters. This is
an additive migration: Phase 0 behavior and its frozen fixtures remain
unchanged.

Repository:

```text
E:\cli\codex-redteam-agent
baseline tag: baseline-v0.1.0
schema version: 4
package requires-python: >=3.11
development/build baseline: Python 3.12.13
```

## Delivered components

### Core contracts

`src/redteam_agent/core/contracts.py` provides the common contract envelope,
canonical JSON serialization, content hashing, JSON-value validation, explicit
schema versions, and forward-version rejection. The domain contracts are
defined under `src/redteam_agent/core/domain/`:

- `Goal` and `GoalCriterion`
- `Intent`, `Budget`, and `Run`
- `SearchNode`
- `Evidence` and `EvidenceProvenance`
- `Finding`, `Asset`, and `AttackPath`
- `TerminalDecision`

The port DTOs and provider-neutral interfaces are under
`src/redteam_agent/core/ports/`:

- `ModelPort`
- `ToolPort`
- `WorkerPort`
- `StorePort`
- `EventPort`

Core imports only Python standard-library modules and its own contract modules.
It has no imports from MCP, Codex, HTTP, SQLite, or a concrete model/tool
provider. The boundary is enforced by `tests/test_core_boundaries.py`.

### Legacy adapters

`src/redteam_agent/adapters/runtime.py` exposes the Core view over the legacy
runtime and bridges store, event, and tool ports to the existing durable
components. `runtime_mapping.py` contains the explicit Legacy/Core mappings.

The adapter preserves the existing Runtime as the authority for control-plane
transitions. A Core store caller cannot forge a terminal status, branch/action
transition, or evidence membership; these mutations are rejected and must go
through the Runtime. CAS remains enforced by `DurableStore`.

### Explicit schema migrations

`src/redteam_agent/runtime/store_migrations.py` introduces a numbered migration
registry and migration reports. The current schema remains version `4`; the
registry applies versions `1..4` in order, records migration history in
`schema_metadata`, rejects a database newer than the runtime, and is safe to
run repeatedly.

Fixed DDL is executed statement-by-statement through
`execute_sql_script()` rather than `sqlite3.executescript()`, preventing an
implicit commit from breaking the surrounding transaction. Existing handoff
schema creation uses the same atomic path.

## Machine-verifiable artifacts

Core contract determinism is captured in:

```text
tests/fixtures/phase1/core_contracts.json
scripts/phase1_snapshot.py
```

The snapshot was checked twice consecutively without rewriting the fixture:

```text
phase1 core contract snapshot verified
phase1 core contract snapshot verified
```

Phase 0's six authoritative snapshots were also checked twice consecutively.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| Core types and ports exist | `src/redteam_agent/core/`; contract and port tests | pass |
| Core has no concrete-provider/runtime imports | `tests/test_core_boundaries.py` | pass |
| Stable serialization/deserialization and version upgrade | `tests/test_core_contracts.py`; Phase 1 snapshot | pass |
| Future schema versions are rejected | contract version tests | pass |
| Legacy Runtime is bridged without replacing it | `tests/test_runtime_core_adapter.py` | pass |
| Core adapter cannot bypass Runtime control transitions | adapter invariant tests | pass |
| Existing SQLite data loads without loss | migration compatibility tests | pass |
| Migration is forward-only and repeatable | `tests/test_store_migrations.py` | pass |
| DDL transaction remains atomic | SQL-script rollback/atomicity tests | pass |
| Existing 142-test behavior remains green | complete suite: `164 passed` | pass |

## Validation record

| Gate | Result |
|---|---|
| Complete regression suite | `164 passed in 27.56s` |
| Python 3.12.13 `compileall` (`src`, `scripts`, `tests`) | passed |
| Phase 0 snapshot check, two consecutive executions | passed |
| Phase 1 snapshot check, two consecutive executions | passed |
| `git diff --check` | passed |
| Python 3.12.13 wheel build | passed |
| Wheel | `codex_redteam_agent-0.1.0-py3-none-any.whl` |
| Wheel SHA-256 | `6fecdc3347fb32f0703698cae10e5e4003d77e866ca8cf5e6aa8b02c5d41e65a` |
| Isolated wheel installation | passed |
| Installed Core imports (`Goal`, `ModelPort`, `StorePort`) | passed |
| Installed adapter import (`OperationRuntimeAdapter`) | passed |
| Installed-package self-test | completed; terminal success |
| MCP initialize | `redteam-agent-runtime`, protocol `2025-06-18` |
| MCP tools/list | five public tools, unchanged order |

The full suite was executed with the available local pytest runner; Python
3.12.13 was used for the compile, wheel, isolated-install, self-test, and MCP
gates. No remote repository was configured or contacted.

## Scope boundary for the next phase

Phase 1 deliberately does not introduce `AgentService`, a new model loop, or a
new state machine. Those belong to Phase 2 and later. `OperationRuntime` stays
available as the compatibility facade until the AgentService migration is
accepted.
