# Phase 0 Acceptance — Authoritative Baseline Freeze

## Result

Phase 0 is accepted. The original runtime is preserved by an independent local
Git repository and an annotated baseline tag. No file under `src/redteam_agent`
was changed during this phase.

```text
repository: E:\cli\codex-redteam-agent
branch: main
baseline tag: baseline-v0.1.0
baseline commit: 5a931a08bca9ed900cf902e384dad190ba4e12d0
baseline tree: 2a1407bd57af43e220a9dea3fb8db7585200b8bd
python development baseline: 3.12
package requires-python: >=3.11
```

The pre-refactor encoding anomalies and runtime behavior remain present in the
tagged baseline. They were deliberately not repaired before the tag.

## Machine-verifiable snapshots

Generate the fixtures:

```powershell
python -m scripts.phase0_snapshot --write tests\fixtures\phase0\snapshots
```

Verify the fixtures without rewriting them:

```powershell
python -m scripts.phase0_snapshot --check tests\fixtures\phase0\snapshots
```

The check was executed twice consecutively and produced the same six documents.

| Snapshot | SHA-256 |
|---|---|
| `runtime_identity.json` | `63a3e9942a185661e0cc73545c155ff742b8eac78a1d7de33b6bf5843cfe6041` |
| `mcp_tools.json` | `cd30d2a31702a6b29c36a5707d3bcb874b058a5ba306588e2ca8fb33cce41d20` |
| `sqlite_schema.json` | `a8e25683328e9e6f6b62d2d6f21911c8c3f18fc681ca6ac129f1b439f02cb4fa` |
| `goal_contract.json` | `22ac63343bd76632f02181f59079a099b3325e55487ccc2d2c9c46156d05691d` |
| `operation_trace.json` | `86db87a3ee009ae83967d48fbfee1aed8d8acc31e1d190a75c615696f4b30cfd` |
| `evidence_terminal.json` | `5a8da617d7b585088f280469d7eb6273aa1465266f106984290e213d92bc1a5d` |

The snapshots cover:

- five public and three legacy MCP tool schemas;
- SQLite schema version and complete DDL;
- deterministic GoalContract and intent rewrite output;
- start-to-terminal durable state transitions and event ordering;
- semantic EvidenceGraph output and TerminalDecision behavior.

## Validation record

| Gate | Result |
|---|---|
| Original regression suite before Phase 0 additions | `142 passed` |
| Complete suite after Phase 0 additions | `145 passed in 17.84s` |
| Python 3.12 `compileall` for `src`, `scripts`, and `tests` | passed |
| Snapshot verification, two consecutive executions | passed |
| Wheel build on Python 3.12.13 | passed |
| Wheel | `codex_redteam_agent-0.1.0-py3-none-any.whl` |
| Acceptance wheel SHA-256 | `5b3ed2370d3faf57e7c9ab701d3beee642fca7a0563a24bcce858ada3dc9a965` |
| Isolated wheel installation | passed |
| Installed package version/path verification | `0.1.0`, isolated target directory |
| Installed-package self-test | `completed`, terminal success |
| MCP initialize | `redteam-agent-runtime`, protocol `2025-06-18` |
| MCP tools/list | five public tools |
| Baseline archive restore probe | 84 entries; required package/runtime files present |

Public MCP tool order is frozen as:

```text
redteam_run
redteam_status
redteam_evidence
redteam_cancel
redteam_events
```

## Regression gates for later phases

Every later phase must satisfy all of the following before acceptance:

1. `python -m pytest`
2. Python 3.12 `compileall`
3. `python -m scripts.phase0_snapshot --check tests\fixtures\phase0\snapshots`
4. wheel build and isolated installation
5. installed-package self-test
6. MCP initialize and tools/list compatibility

An intentional compatibility change requires a reviewed fixture update in the
same commit and an explanation in that phase's acceptance document. A changed
snapshot by itself is a regression, not an approval to overwrite the baseline.
