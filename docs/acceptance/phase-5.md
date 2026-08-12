# Phase 5 Acceptance - Worker Plane, Artifact Store and Context Projection

## Result

Phase 5 is accepted. Complete worker output is stored in a SHA-256
content-addressed store, while SQLite keeps bounded projections, immutable
run-bound references and lineage. Local, MCP, Codex handoff and Docker adapter
boundaries implement the existing `WorkerPort`; `AgentService` remains the only
application-layer write entrypoint.

Token reduction is capability-preserving. It changes only the model projection:
the append-only transcript, complete Artifact bytes, verified Evidence and
protected Goal state remain available to Runtime and Verifier. If protected
context exceeds a provider window, the selector records
`context_overflow_tokens` instead of deleting Goal clauses or Evidence.

## Durable schema

Schema migrations 7 and 8 add:

```text
artifact_blobs
artifact_refs
artifact_links
artifact_fts
run_workspaces
worker_tasks
```

`artifact_blobs` deduplicates complete bytes by SHA-256 across runs.
`artifact_refs` binds each semantic reference to one run and one Artifact type.
Parent and child lineage both use `(artifact_id, run_id)` foreign keys, so an
Artifact from another run cannot become a parent. Read operations verify both
the recorded byte count and SHA-256 digest.

Preview and metadata JSON are bounded before SQLite persistence. FTS5 indexes
only the redacted preview/metadata projection and every query includes the
authoritative `run_id` predicate.

## Worker plane

- `LocalWorker` accepts an explicit argv sequence with `shell=False`, writes
  stdout/stderr to files, then streams those files into CAS.
- Each run receives a SHA-256-derived workspace and a restricted environment;
  parent environment variables are not inherited except for the platform
  allowlist and explicit per-task overlays.
- Timeout and cancellation terminate the process tree. The task record reaches
  one durable terminal status: `completed`, `failed`, `timed_out`, `cancelled`
  or `unavailable`.
- A `(run_id, worker_kind, idempotency_key)` may identify only one payload.
  Restarted services return the existing result without repeating side effects.
- `McpWorker` stores the complete `ToolResult` as an Artifact and persists
  transport/artifact failures as failed worker results.
- `CodexHandoffWorker` exposes a durable `waiting_worker` projection with replay
  protection. `DockerWorkerAdapter` is pluggable and reports `unavailable`
  until configured; it never reports false success.

## Context and token behavior

The model request prefix is stable:

```text
system invariant
protected Goal / plan / Evidence context
traceable summary when it fits
recent atomic turns
current action delta
```

Recent messages are selected against `ModelCapabilities.max_context_tokens`.
Provider-reported input usage is preferred when present; missing usage uses a
conservative UTF-8 byte estimate for projection only and never updates the
authoritative token budget. Assistant tool calls and their tool results share a
request group and cannot be split. Large tool results become bounded Artifact
projections in the transcript and model observation; recovery verifies and
loads complete data from CAS.

Recorded request metrics are:

```text
estimated_context_tokens
provider_context_tokens
selected_tokens
reserved_output_tokens
projection_bytes
cache_read_tokens
cache_write_tokens
context_overflow_tokens
```

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| CAS tamper/truncation detection | `test_artifact_detects_tampering_truncation_and_blob_metadata_corruption` | pass |
| Blob deduplication with run-bound refs | `test_artifact_blob_deduplicates_while_references_remain_run_bound` | pass |
| Same bytes support distinct semantic refs | `test_artifact_same_content_can_have_multiple_semantic_refs_and_lineage` | pass |
| Bounded redacted SQLite projection | `test_artifact_metadata_and_preview_are_bounded_before_sqlite_persistence` | pass |
| FTS run isolation | `test_artifact_projection_is_bounded_redacted_and_search_is_run_scoped` | pass |
| Large output does not grow OperationState | `test_local_worker_persists_bounded_projection_and_full_artifacts` | pass |
| Restart reconcile is idempotent | `test_local_worker_restart_reconciles_without_duplicate_execution` | pass |
| Payload-bound idempotency | `test_worker_idempotency_key_rejects_changed_payload` | pass |
| Timeout and cancel propagation | `test_worker_timeout_terminates_process_and_records_terminal_result`; `test_worker_cancel_propagates_to_active_process` | pass |
| Workspace and environment isolation | `test_workspace_environment_isolated_between_runs` | pass |
| Workspace/path and Artifact scope | `test_workspace_rejects_relative_escape_and_cross_run_required_artifact` | pass |
| MCP durable Artifact and failure state | `test_mcp_worker_persists_full_result_artifact_and_replays`; `test_mcp_worker_exception_becomes_durable_failed_result` | pass |
| Codex replay and Docker gap semantics | `test_codex_handoff_replay_and_cancel_are_persistent`; `test_docker_adapter_reports_configured_capability_gap_without_success` | pass |
| Atomic tool-call/result context | `test_context_budget_keeps_assistant_tool_turn_atomic` | pass |
| Complete large result remains readable | `test_large_tool_result_is_projected_but_complete_cas_remains_readable` | pass |
| Stable prefix and projection metrics | `test_model_request_records_context_projection_metrics_and_stable_prefix` | pass |
| Adversarial usage rejected | `test_adversarial_provider_usage_is_rejected` | pass |
| SQLite, Blob, lineage and FTS tampering | `test_artifact_sqlite_column_tampering_is_detected`; `test_artifact_blob_lineage_and_fts_tampering_are_detected` | pass |
| Worker record and crashed-execution recovery | `test_worker_record_tampering_is_detected_before_replay`; `test_crashed_running_local_worker_becomes_unknown_without_reexecution` | pass |
| Worker Artifact truth and scoped cancellation | `test_worker_required_and_replayed_artifacts_require_valid_cas_bytes`; `test_worker_cancel_routes_only_to_the_owning_adapter`; `test_worker_status_and_cancel_reject_cross_run_task_access` | pass |
| Malicious task ID containment | `test_malicious_task_id_cannot_control_workspace_paths` | pass |
| Unverified tactical state retention | `test_old_unverified_hypothesis_survives_extreme_context_pressure` | pass |
| Complete and interrupted stream floods | `test_stream_flood_is_content_addressed_and_sqlite_events_are_bounded`; `test_interrupted_stream_flood_is_diagnostic_only_and_content_addressed` | pass |
| Tampered model-observation CAS recovery | `test_tampered_cas_model_observation_blocks_recovery` | pass |

The directory-symlink escape test is skipped on the current Windows session
because directory symlink creation is unavailable. The same containment path is
exercised by the relative path escape test, and the implementation rejects an
existing symlink/reparse path before execution.

## Machine-verifiable artifacts

```text
scripts/phase5_snapshot.py
tests/fixtures/phase5/worker_artifacts.json
tests/test_phase5_snapshot.py
```

The Phase 5 fixture includes schema, public method signatures, adapter
protocols, context metrics and a real LocalWorker vertical fixture. It was
generated once and checked twice byte-for-byte. Phase 0 through Phase 4
historical snapshots continue to pass.

## Validation record

| Gate | Result |
|---|---|
| Development/build baseline | Python 3.12.13 |
| Complete regression suite | 256 passed, 1 environment-only symlink skip |
| Phase 5 adversarial suite | 17 passed |
| `compileall` (`src`, `scripts`, `tests`) | pass |
| Phase 0/1/2/3/4/5 snapshots | pass; Phase 5 checked twice |
| Wheel build | pass; SHA-256 `1168b5bc5e3e825af0f91940bbb6e7e30312847d163dcd92e283af00f20c1a03` |
| Isolated wheel installation | pass on Python 3.12.13 |
| Installed-package self-test | `completed`; terminal success |
| MCP initialize | `redteam-agent-runtime`, protocol `2025-06-18` |
| MCP tools/list | `redteam_run`, `redteam_status`, `redteam_evidence`, `redteam_cancel`, `redteam_events` |

The wheel was rebuilt from the final Phase 5 source with Python 3.12.13,
`setuptools 83.0.0` and `wheel 0.47.0`, without network access.
