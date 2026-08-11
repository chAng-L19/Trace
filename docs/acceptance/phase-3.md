# Phase 3 Acceptance - Provider-Agnostic ModelLoop

## Result

Phase 3 is accepted. `AgentService` can now drive a provider-agnostic
`ModelLoop` while `OperationRuntime` retains exclusive authority over run
state, Evidence promotion, budgets, cleanup, and terminal decisions.

Repository baseline:

```text
root: E:\cli\codex-redteam-agent
parent: a2193617f7d2314a4c262bd7c31f03f5d1344443
schema version: 5
requires-python: >=3.11
validation runtime: Python 3.12.13
```

## Architecture

`src/redteam_agent/application/model_loop.py` implements the orchestration
loop. Providers implement only `ModelPort`; tools implement only `ToolPort`.
The application layer constructs native system/user messages, capability-aware
tool definitions, structured output schemas, parallel-call policy, retry and
streaming behavior. Provider adapters do not own lifecycle transitions.

Every model attempt persists an immutable request record containing:

- Runtime-computed prompt SHA-256
- Provider and model identity
- Declared capabilities
- Native messages, tools, response schema, and request metadata

Every response persists the Runtime-computed response SHA-256, the Provider's
claimed hash, normalized usage when reported, and the complete response
contract. Claimed response and tool hashes are checked before any result can
advance the run.

## Observation Boundary

Model text and structured output are durable model records, not Evidence.
Every Tool Result first becomes a `ModelObservationRecord`. Only after that
append-only record exists may `AgentService` submit an Observation to the
existing Runtime verifier path. Model-authored `evidence` and `terminal`
objects have no mutation path into `EvidenceGraph` or `TerminalJudge`.

The schema migration adds:

```text
model_requests
model_responses
model_stream_events
model_observations
```

Migration 5 is forward-only, atomic, and idempotent. Phase 0 continues to
project its frozen v4 baseline object set; the current v5 schema is frozen in
the Phase 3 snapshot.

## Recovery

The loop recovers the latest completed turn for the current action by
revalidating the persisted prompt hash, response hash, call identities, and
Observation hashes. Missing tool calls use `ToolPort.reconcile()` before
`invoke()`. Already recorded Tool Results are reused in original call order.

The crash tests prove both critical boundaries:

1. Process loss after a response commit resumes the missing tool call without
   issuing another model request.
2. Process loss after a Tool Observation commit reuses that Observation and
   does not repeat the tool side effect.

## Acceptance Matrix

| Requirement | Evidence | Result |
|---|---|---|
| Native system role and structured output | `test_text_and_structured_responses_are_durable_but_not_evidence` | pass |
| Serial Tool Call | installed-wheel end-to-end test and `test_model_loop_completes_a_recoverable_end_to_end_run` | pass |
| Parallel Tool Calls | barrier-backed `test_parallel_tool_calls_are_all_captured_as_observations` | pass |
| Streaming and usage | `test_streaming_completion_preserves_events_and_usage` | pass |
| Streaming interruption | `test_stream_interruption_is_durable_and_retryable` | pass |
| Retry with distinct durable attempts | `test_transient_provider_failure_retries_with_a_new_request` | pass |
| Cancellation reaches active Provider | `test_model_loop_cancel_propagates_to_active_provider_request` | pass |
| Provider switch preserves Goal/Evidence/Terminal semantics | `test_provider_switch_does_not_change_goal_evidence_or_terminal_semantics` | pass |
| Model cannot write Evidence or terminal state | `test_model_cannot_inject_direct_evidence` | pass |
| Claimed Provider and Tool hashes are not trusted | response/tool hash integrity tests | pass |
| Crash after model response is recoverable | `test_restart_after_durable_model_response_resumes_missing_tool_call` | pass |
| Crash after Tool Observation does not repeat effect | `test_restart_after_durable_tool_observation_does_not_repeat_side_effect` | pass |

## Machine-Verifiable Artifacts

```text
scripts/phase3_snapshot.py
tests/fixtures/phase3/model_loop.json
tests/test_phase3_snapshot.py
```

Snapshot SHA-256:

```text
7f19350e7691341bb8c6eeffa851041db1983646ce0fae2d295d289669e56b53
```

## Validation Record

| Gate | Result |
|---|---|
| Complete regression suite | `200 passed in 37.45s` |
| Python 3.12.13 compileall (`src`, `scripts`, `tests`) | passed |
| Phase 0/1/2 snapshot checks | passed |
| Phase 3 snapshot check, two consecutive executions | passed |
| `git diff --check` | passed |
| Python 3.12.13 wheel build | passed |
| Wheel SHA-256 | `dcb1d8e337c911b9c05c126a0db93804f63382f3b3722030f74a539995b99551` |
| Isolated wheel installation | passed |
| Installed Fake Provider run | `completed`, terminal success, one durable Observation |
| Installed legacy self-test | `completed`, terminal success |
| MCP initialize | `redteam-agent-runtime`, protocol `2025-06-18` |
| MCP tools/list | five public tools, unchanged order |

The wheel was built without network access using Python 3.12.13 and the local
PEP 517 backend (`pip wheel --no-build-isolation --no-cache-dir`).

## Next Boundary

Phase 3 does not compact conversation context or store interrupted stream data
as content-addressed diagnostic Artifacts. Phase 4 adds complete transcript
persistence, protected context, traceable summaries, and authoritative
action/token/time budget accounting.
