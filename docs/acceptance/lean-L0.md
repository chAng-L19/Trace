# Lean L0 Acceptance - Complexity and Behavior Freeze

## Scope

L0 audits complexity, local import boundaries, deletion-candidate references and
historical behavior. It intentionally deletes no production code.

## Reproducible audit

```powershell
python scripts/lean_l0_audit.py
python -m pytest tests/test_lean_l0_audit.py
```

Machine-readable output: `docs/acceptance/lean-l0-audit.json`.

## Baseline measurements

```text
production Python files: 99
production Python lines: 23,794
largest file: src/redteam_agent/application/model_loop.py (796 lines)
files over 800 lines: 0
Python baseline: 3.12
```

The report is deterministic across two consecutive generations. It records
local import edges and source/test references for every deletion candidate.

## Deletion candidates

| Candidate | Current references | L0 result |
|---|---:|---|
| `OperationRuntimeAdapter` | 4 files | defer to L1 compatibility migration |
| `RuntimeStoreAdapter` | 2 files | defer to L1 projection consolidation |
| `RuntimeEventAdapter` | 2 files | defer to L1 projection consolidation |
| `RuntimeToolAdapter` | 3 files | retain until AgentService ToolPort contract is canonical |
| `AdaptivePlanner` | 9 files | defer to L3 `NextActionPolicy` replacement |
| `Scheduler` | 4 files | defer to L3 terminal/remediation coverage |
| `WorkflowRegistry` | 11 files | retain as compatibility shim until L10 |
| `TacticalLoopMixin` | 2 files | defer to L3 single AgentLoop merge |

Reference counts include production and test files. The audit also records the
exact per-file counts in JSON; the audit test itself is excluded from deletion
decisions during review.

## Behavior freeze

```text
Phase 0 snapshot: verified
Phase 1 snapshot: verified
Phase 2 snapshot: verified
Phase 3 snapshot: verified
Phase 4 snapshot: verified
Phase 5 snapshot: verified
Phase 6 snapshot: verified
targeted L0 tests: 2 passed
full regression: 282 passed, 1 skipped
compileall: passed
```

## L0 decision

L0 passes. The first safe deletion work is L1: make `AgentService` the only
application entry, migrate `test_runtime_core_adapter.py` to canonical contract
tests, then reduce `OperationRuntimeAdapter` to a forwarding shim. No candidate
is deleted before its replacement path and recovery/lineage tests pass.
