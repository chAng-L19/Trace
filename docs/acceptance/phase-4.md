# Phase 4 Acceptance — Conversation, Context and Budget

## Result

Phase 4 is accepted. Conversation history, context selection/compaction and
action/token/time budgets are durable Runtime concerns. The model loop can use
the selected context, but it cannot delete source messages, manufacture token
usage, or promote interrupted output into a response, Evidence, or terminal
decision.

## Durable records

Schema migration 6 adds `conversation_messages`, `context_summaries`,
`context_snapshots`, `diagnostic_artifacts`, and `model_budget_usage`. All
records are append-only or immutable-by-hash. SQLite retains metadata and
lineage; raw source messages remain available after compaction.

## Protected context

Every selection permanently carries the original Goal, unsatisfied clauses,
active plan, critical verified Evidence references, and irreversible state.
Selection writes a content-addressed snapshot, so the exact model projection
can be replayed and audited after a restart.

## Budget semantics

Action, token, and absolute wall-clock deadline checks occur in Runtime before
side effects. Model usage is charged once per `request_id`; missing usage is
represented as unknown and pauses a run with `token_usage_unknown` until an
explicit budget acknowledgement. A paused run is resumable and is not a
successful terminal state.

## Streaming boundary

An interrupted stream stores its partial text only as a diagnostic artifact.
The response text remains empty, no assistant transcript is appended, and no
Evidence or terminal state can be derived from the partial output.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| Complete system/user/assistant/tool transcript | `test_complete_system_user_assistant_tool_transcript_is_persistent` | pass |
| Append-only compaction with source lineage | `test_compaction_is_append_only_and_summary_has_source_lineage` | pass |
| Protected context at zero selection budget | `test_protected_context_survives_zero_message_selection` | pass |
| Protected messages cannot be compacted | `test_protected_messages_cannot_be_compacted` | pass |
| Token exhaustion pauses before tool side effect | `test_token_limit_pauses_before_tool_and_resumes_from_durable_response` | pass |
| Missing usage is not fabricated | `test_missing_usage_is_not_fabricated_and_requires_explicit_acknowledgement` | pass |
| Usage accounting is idempotent | `test_model_usage_commit_is_idempotent_per_request` | pass |
| Absolute deadline is durable | `test_expired_absolute_deadline_pauses_before_provider_call` | pass |
| Partial stream is diagnostic-only | `test_interrupted_stream_partial_is_only_a_diagnostic_artifact` | pass |

## Machine-verifiable artifacts

```text
scripts/phase4_snapshot.py
tests/fixtures/phase4/context_budget.json
tests/test_phase4_snapshot.py
```

The fixture is generated twice and compared byte-for-byte. Phase 0 and Phase 3
snapshot scripts continue projecting their historical v4/v5 schema contracts.

## Validation record

| Gate | Result |
|---|---|
| Complete regression suite | 212 passed |
| Python 3.12.13 compileall (`src`, `scripts`, `tests`) | passed |
| Phase 0/1/2/3/4 snapshots | passed; Phase 4 checked twice byte-for-byte |
| Wheel build | passed; SHA-256 `684ae4b6284cd73f3cc51c91e6ab0c08d6648b8c88af79b05e2f8ef07ef1d930` |
| Isolated wheel installation and self-test | passed; terminal success |
| MCP initialize | `redteam-agent-runtime`, protocol `2025-06-18` |
| MCP tools/list | five public tools, unchanged order |
