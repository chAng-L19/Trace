# Phase 2 Acceptance - AgentService and Durable Lifecycle

## Result

Phase 2 is accepted. `AgentService` is now the canonical application entry
point for starting, running, observing, inspecting, cancelling, and streaming
events from durable agent runs. The existing `OperationRuntime` API remains
available as the compatibility facade while MCP, Codex, and CLI convergence is
deferred to Phase 9.

Repository:

```text
E:\cli\codex-redteam-agent
baseline tag: baseline-v0.1.0
schema version: 4
package requires-python: >=3.11
development/build baseline: Python 3.12.13
```

## AgentService surface

The application entry point is defined in
`src/redteam_agent/application/agent_service.py`:

```python
class AgentService:
    def start(self, request): ...
    def run(self, run_id, budget_delta=None, *, max_actions=None): ...
    def submit_observation(self, run_id, observation): ...
    def status(self, run_id): ...
    def cancel(self, run_id, reason="user_requested"): ...
    def events(self, run_id, after_sequence=0, *, limit=200): ...
```

Typed application contracts are provided by
`src/redteam_agent/application/contracts.py`:

- `StartRequest`
- `BudgetDelta`
- `Observation`
- `AgentRunView`
- `AgentStartResult`

`AgentService.start()` automatically uses the existing batch protocol when a
request contains multiple targets. Each target receives a distinct `run_id`,
GoalContract, PlanRevision, Fact namespace, Evidence namespace, and terminal
decision.

## Canonical lifecycle

The public lifecycle is fixed to:

```text
created
running
waiting_worker
paused_budget
cancelling
cancelled
completed
failed
```

Legacy execution detail remains available in `Run.metadata["legacy_status"]`.
The application projection maps `waiting_goal_input`, `waiting_host`,
`waiting_tools`, and `waiting_dependency` to `waiting_worker`; integrity failure
maps to `failed`. Every AgentService mutation validates the observed transition
against `ALLOWED_RUN_TRANSITIONS` and requires an authoritative terminal
decision for `completed`, `failed`, and `cancelled` states.

## Durable idempotency and recovery

Budget changes with an explicit idempotency key are committed through
`ServiceStoreMixin.apply_budget_delta_once()`. The state update, immutable
command result, event, and state snapshot share one SQLite transaction. The raw
client key is not persisted; a run-scoped SHA-256 key is stored instead. A
replay returns the current state without applying the delta again, while the
same key with different input is rejected.

External Observations accept an explicit idempotency key. The key is scoped to
the run, branch, plan revision, and action. Existing TaskAttempt and
ActionResult records are reused so a process restart can reconcile a cached
result without repeating a tool side effect or creating duplicate Evidence.

No SQLite schema change was required. Schema version `4` and all Phase 0 DDL
fixtures remain unchanged.

## Concurrency and terminal authority

AgentService delegates operation ownership to the existing operation lease and
action leases. Fencing tokens reject stale workers, state CAS selects one
valid writer, and `commit_action_outcome()` remains the only atomic promotion
path for Attempt, Observation, Evidence, Fact, Review, Plan, Event, and state.

If cancellation wins while an action is completing, AgentService recognizes
the control-plane conflict after the operation lease is released and converges
through the cancellation path. The acceptance race produces exactly one
authoritative terminal event and no losing final-report Evidence.

## Machine-verifiable artifacts

The Phase 2 public interface and lifecycle are frozen in:

```text
tests/fixtures/phase2/agent_service.json
scripts/phase2_snapshot.py
```

Snapshot SHA-256:

```text
41f395adb55c6c9b0f5096c5f691e34b6d4c08fc33c685e8c131e78748355d5b
```

The Phase 0, Phase 1, and Phase 2 snapshot checks were each executed twice
consecutively without rewriting their fixtures.

## Acceptance matrix

| Requirement | Evidence | Result |
|---|---|---|
| AgentService is the canonical application API | Phase 2 snapshot and `test_agent_service_exposes_the_canonical_lifecycle` | pass |
| Public status is limited to the eight canonical states | lifecycle snapshot and waiting-state projection tests | pass |
| Restart before every deterministic fixture action is recoverable | parameterized `test_restart_before_each_action_recovers_exact_boundary` | pass |
| Restart after cached tool output is recoverable | parameterized `test_restart_after_cached_action_result_reconciles_without_duplicate_evidence` | pass |
| Same idempotency key does not duplicate budget, tool effects, or Evidence | budget, Observation, and counting-tool recovery tests | pass |
| Concurrent recovery has one legal committer | `test_concurrent_recovery_has_one_submitter_and_one_tool_side_effect` | pass |
| Cancel/complete race has one authoritative terminal | `test_cancel_and_completion_race_has_one_authoritative_terminal` | pass |
| Multi-target Fact and Evidence namespaces do not cross | `test_multi_target_start_creates_isolated_runs_and_evidence` | pass |
| Credential references do not cross between runs | `test_runs_do_not_share_credential_references` | pass |
| Existing OperationRuntime callers remain compatible | complete regression suite and facade boundary test | pass |

The crash matrix covers every executable action in the deterministic plan-only
fixture. The post-call crash is injected after the tool result is cached but
before the atomic action outcome commit, exercising the real recovery path.

## Validation record

| Gate | Result |
|---|---|
| Complete regression suite | `183 passed in 26.92s` |
| Python 3.12.13 `compileall` (`src`, `scripts`, `tests`) | passed |
| Phase 0 snapshot check, two consecutive executions | passed |
| Phase 1 snapshot check, two consecutive executions | passed |
| Phase 2 snapshot check, two consecutive executions | passed |
| `git diff --check` | passed |
| Python 3.12.13 wheel build | passed |
| Wheel | `codex_redteam_agent-0.1.0-py3-none-any.whl` |
| Wheel SHA-256 | `3f6233ae6f81c624092d5ed211efe097d060581db888440d19547e71b209b363` |
| Isolated wheel installation | passed |
| Installed AgentService run | `completed`, terminal success, three Evidence nodes |
| Installed-package legacy self-test | `completed`, terminal success |
| MCP initialize | `redteam-agent-runtime`, protocol `2025-06-18` |
| MCP tools/list | five public tools, unchanged order |

The full pytest suite used the available local pytest runner. Python 3.12.13
was used for compile, snapshots, wheel build, isolated installation,
AgentService execution, self-test, and MCP compatibility checks. No remote
repository was configured or contacted.

## Scope boundary for the next phase

Phase 2 does not add a provider model loop or move orchestration into Provider
adapters. Phase 3 will introduce the provider-agnostic `ModelLoop` on top of
this AgentService and the Phase 1 ports.
