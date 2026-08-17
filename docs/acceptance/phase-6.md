# Phase 6 Acceptance - Thin Tactical Loop, Visibility and Anti-False-Falsification

## Result

Phase 6 replaces scheduler-heavy tactical planning with a model-led loop. The
legacy `generic-adaptive` workflow remains available to Phase 0–5 callers as a
compatibility quality-gate path, but it no longer defines the primary model's
search space. The model may execute multiple arbitrary tool actions inside a
gate and advances the gate only when it explicitly commits tool-derived output.

## Tactical authority boundary

- The primary model creates hypotheses, priorities, local search order and
  reopening decisions.
- Runtime owns scope, credentials, action/token/time budget, idempotency,
  Artifact integrity, Evidence promotion, cleanup and terminal decisions.
- `SearchGraph` semantics are implemented as an append-only
  `ExplorationLedger`; entries are navigation state and never Evidence.
- Repeated actions generate stagnation diagnostics without closing a branch or
  terminating the run.

## Anti-false-falsification

The ledger separates:

```text
ObservedMiss
CoverageClaim
HypothesisState
VerifiedNegative
```

An `ObservedMiss` must record the exact tested domain, observations, coverage
boundary, uncertainty and reopening triggers. It cannot use `closed` status.
A `VerifiedNegative` additionally requires a raw Artifact or verified Evidence
reference and positive confidence. New evidence, Artifact or capability signals
matching a reopening trigger append an automatic `reopen` record; the old record
is retained unchanged.

## Tool visibility and token behavior

Every model tool result is persisted as complete JSON in SHA-256 CAS. The model
transcript receives a bounded projection containing, when present:

```text
status_code
headers
body byte count/hash/preview
timing
request method/url/path
baseline/observed differences
enumeration counts/samples/coverage
raw Artifact reference
```

The projection is a navigation aid. Runtime verification and later forensic
review can always load the complete Artifact.

## Context continuity

The existing single primary-model transcript remains authoritative. Context
compaction now creates a traceable `ReconDigest` at a degradation boundary. The
digest includes targets, lifecycle state, raw Artifact references, attempted
actions, confirmed Evidence, unverified hypotheses, contradictions, reopening
triggers and repeated-action signals. It stores source message/record hashes and
is explicitly marked non-authoritative.

## Durable schema

Migration 9 adds:

```text
exploration_records
recon_digests
tactical_attempts
```

All records are run-bound and append-only. Tactical attempts are idempotent by
`(run_id, request_id, call_id)` and are included in authoritative action-budget
reconciliation after a restart.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| Observed miss cannot close a direction | `test_observed_miss_never_closes_a_hypothesis_or_becomes_evidence` | pass |
| Negative conclusion requires scoped coverage/source | `test_verified_negative_is_scoped_and_new_capability_reopens_it` | pass |
| New capability reopens a closed branch | `test_verified_negative_is_scoped_and_new_capability_reopens_it` | pass |
| Model can search repeatedly inside one quality gate | `test_model_led_tactical_loop_can_probe_without_advancing_the_quality_gate` | pass |
| Duplicate action is diagnostic, not terminal | `test_model_led_tactical_loop_can_probe_without_advancing_the_quality_gate` | pass |
| Raw tool result remains complete in CAS | `test_tool_projection_is_bounded_and_complete_raw_result_is_retrievable` | pass |
| Model projection exposes HTTP/API differences | `test_tool_projection_is_bounded_and_complete_raw_result_is_retrievable` | pass |
| Context degradation creates traceable ReconDigest | `test_context_degradation_builds_traceable_recon_digest_and_recovers` | pass |
| Recovery preserves tactical state | `test_context_degradation_builds_traceable_recon_digest_and_recovers` | pass |
| Cross-run Artifact and parent references are rejected | `tests/test_phase6_adversarial.py` | pass |
| Exploration and ReconDigest tampering is detected | `tests/test_phase6_adversarial.py` | pass |
| Tactical replay, payload conflict and budget double-counting are rejected | `tests/test_phase6_adversarial.py` | pass |
| Restart preserves branch reopening state | `tests/test_phase6_adversarial.py` | pass |
| Incomplete enumeration cannot close the wider direction | `tests/test_phase6_adversarial.py` | pass |
| Historical Phase 0–5 behavior remains compatible | complete regression and historical snapshots | pass |

## Machine-verifiable artifacts

```text
scripts/phase6_snapshot.py
tests/fixtures/phase6/thin_tactical_loop.json
tests/test_phase6_snapshot.py
tests/test_phase6_tactical_loop.py
tests/test_phase6_adversarial.py
```

## Validation record

| Gate | Result |
|---|---|
| Python 3.12 complete regression | `270 passed, 1 skipped` |
| Python 3.12 `compileall` for `src`, `scripts`, and `tests` | passed |
| Phase 0 snapshots, two consecutive executions | six files verified; identical |
| Phase 1–6 snapshots | passed |
| Phase 6 snapshot, two consecutive executions | passed; identical |
| Wheel build | `codex_redteam_agent-0.1.0-py3-none-any.whl` |
| Wheel SHA-256 | `3a38937bd4a78d9acdd5f6cb4309945591907836e553ba79e6d80f90dca071cf` |
| Isolated wheel installation | Python 3.12.13; passed |
| Installed-package self-test | `completed`; terminal success; 3 Evidence nodes |
| MCP initialize | `redteam-agent-runtime`; protocol `2025-06-18` |
| MCP tools/list | five public tools; schema-compatible |

The single skipped test is the existing Windows symbolic-link isolation probe;
the platform denied symbolic-link creation before the assertion path. Pytest's
default user temp directory was also inaccessible during the final run, so the
successful regression used a unique `--basetemp` under the repository `.tmp`
directory. This changes only test workspace placement.

## Evaluation metrics introduced

- attack-path completion rate;
- first-touch latency;
- newly exposed nodes per action/token;
- false-falsification rate;
- branch reopening recall;
- repeated-action rate;
- raw-evidence retrieval after compaction;
- context-segment continuity under equal token and wall-clock budgets.
