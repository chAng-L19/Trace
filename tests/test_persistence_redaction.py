from __future__ import annotations

import hashlib
import json
import sqlite3
import sys
from dataclasses import replace
from pathlib import Path


CODEX_ROOT = Path(__file__).resolve().parents[1] / "codex"
if str(CODEX_ROOT) not in sys.path:
    sys.path.insert(0, str(CODEX_ROOT))

from redteam_agent.runtime.mcp_server import RuntimeMcpServer
from redteam_agent.runtime.goal_compiler import GoalCompiler
from redteam_agent.runtime.models import SuccessPredicate, ToolCallResult
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.models import RunBudget
from redteam_agent.runtime.security import (
    CredentialVault,
    canonicalize_sensitive_text,
    project_sensitive,
    redact_sensitive,
    secret_reference,
)


def test_goal_credentials_are_absent_from_state_events_and_mcp_output(tmp_path: Path) -> None:
    bearer = "SUPER_SECRET_TOKEN_1234567890"
    api_key = "sk-1234567890abcdefghijklmnop"
    objective = f"Inspect https://target.invalid with Authorization: Bearer {bearer}; report results"
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)

    state = runtime.start(
        session_id="secret-persistence",
        objective=objective,
        starting_context={"api_key": api_key, "note": f"token={bearer}"},
    )

    database_text = runtime.store.path.read_bytes().decode("utf-8", errors="ignore")
    with sqlite3.connect(runtime.store.path) as connection:
        persisted = "\n".join(
            str(value)
            for row in connection.execute(
                "SELECT state_json FROM operations UNION ALL SELECT payload_json FROM operation_events"
            ).fetchall()
            for value in row
        )
    events = RuntimeMcpServer(runtime)._call_tool(
        "redteam_events",
        {"run_id": state.run_id},
    )["structuredContent"]
    rendered_events = json.dumps(events, ensure_ascii=False)
    loaded = runtime.store.load_operation(state.run_id)
    canonical_objective, redaction = canonicalize_sensitive_text(objective)
    status = runtime.status(state.run_id)
    resumed = runtime.resume(state.run_id, max_actions=1)
    rendered_runtime = json.dumps(
        {
            "status": status.summary(),
            "resume": resumed.summary(),
            "mcp_status": RuntimeMcpServer(runtime)._call_tool(
                "redteam_status",
                {"run_id": state.run_id},
            )["structuredContent"],
        },
        ensure_ascii=False,
        default=str,
    )

    assert loaded is not None
    assert loaded.goal.objective == canonical_objective
    assert "; report results" in loaded.goal.objective
    assert loaded.goal.intent_envelope["source_sha256"] == hashlib.sha256(
        canonical_objective.encode("utf-8")
    ).hexdigest()
    assert loaded.goal.intent_envelope["input_redaction"] == redaction
    assert loaded.goal.intent_envelope["lossless"] is False
    assert loaded.goal.intent_envelope["source_representation"] == "secret-reference-v1"
    assert "Original objective (verbatim):" not in loaded.goal.intent_envelope["execution_prompt"]
    assert redaction["original_sha256"] == hashlib.sha256(objective.encode("utf-8")).hexdigest()
    assert status.state.status == "running"
    assert resumed.state.status == "waiting_host"
    assert status.terminal.reason != "prompt_rewrite_contract_mismatch"
    assert resumed.terminal.reason != "prompt_rewrite_contract_mismatch"
    for secret in (bearer, api_key):
        assert secret not in database_text
        assert secret not in persisted
        assert secret not in rendered_events
        assert secret not in rendered_runtime


def test_non_sensitive_objective_keeps_verbatim_text_and_rewrite_fingerprint(tmp_path: Path) -> None:
    objective = "Inspect https://target.invalid/api; preserve exact formatting; write the report"
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)

    state = runtime.start(session_id="verbatim-persistence", objective=objective)
    loaded = runtime.store.load_operation(state.run_id)

    assert loaded is not None
    assert loaded.goal.objective == objective
    assert loaded.goal.intent_envelope["source_text"] == objective
    assert loaded.goal.intent_envelope["input_redaction"] == {
        "applied": False,
        "representation": "original-source",
        "original_sha256": hashlib.sha256(objective.encode("utf-8")).hexdigest(),
        "original_bytes": len(objective.encode("utf-8")),
        "canonical_sha256": hashlib.sha256(objective.encode("utf-8")).hexdigest(),
        "canonical_bytes": len(objective.encode("utf-8")),
        "credential_refs": [],
    }


def test_budget_token_metadata_is_not_treated_as_a_credential() -> None:
    budget = RunBudget.create(action_limit=64).to_dict()
    redacted = redact_sensitive({"budget": budget})["budget"]

    assert redacted["token_limit"] is None
    assert redacted["tokens_used"] is None
    assert redacted["token_usage_missing"] == 0
    assert RunBudget.from_dict(redacted).to_dict() == budget


def test_goal_compiler_redacts_every_structured_input_before_identity_generation() -> None:
    token = "sk-1234567890abcdefghijklmnop"
    goal = GoalCompiler().compile(
        "Inspect the supplied target and report",
        targets=(f"https://target.invalid/api?api_key={token}",),
        starting_context={"api_key": token, "note": f"token={token}"},
        constraints={"authorization": f"Bearer {token}"},
        success_predicates=(
            SuccessPredicate(
                kind="artifact_verified",
                subject="final_report",
                description=f"confirm token={token} is exercised",
            ),
        ),
    )

    rendered = json.dumps(goal.to_dict(), ensure_ascii=False, default=str)
    token_ref = secret_reference(token)
    assert token not in rendered
    assert "[SECRET_REF:sha256:" in rendered
    assert goal.targets == (f"https://target.invalid/api?api_key={token_ref}",)
    assert goal.starting_context["api_key"] == token_ref
    assert goal.constraints["authorization"] == secret_reference(f"Bearer {token}")
    assert goal.success_predicates[0].description == f"confirm token={token_ref} is exercised"


def test_secret_reference_round_trip_preserves_nested_sensitive_keys(tmp_path: Path) -> None:
    bearer = "BEARER_SECRET_1234567890"
    api_key = "sk-1234567890abcdefghijklmnop"
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="nested-secret-roundtrip",
        objective=f"Inspect https://target.invalid with Bearer {bearer}",
        starting_context={
            "outer": {
                "api_key": api_key,
                "headers": {"Authorization": f"Bearer {bearer}"},
            }
        },
        constraints={"nested": {"access_token": bearer}},
    )

    loaded = runtime.store.load_operation(state.run_id)
    assert loaded is not None
    outer = loaded.goal.starting_context["outer"]
    assert outer["api_key"] == secret_reference(api_key)
    assert outer["headers"]["Authorization"] == secret_reference(f"Bearer {bearer}")
    assert loaded.goal.constraints["nested"]["access_token"] == secret_reference(bearer)
    assert set(loaded.credential_refs) == {
        secret_reference(api_key),
        secret_reference(bearer),
        secret_reference(f"Bearer {bearer}"),
    }
    rendered = runtime.store.path.read_bytes().decode("utf-8", errors="ignore")
    assert bearer not in rendered
    assert api_key not in rendered
    assert "[REDACTED sha256:" not in json.dumps(loaded.to_dict(), ensure_ascii=False)


def test_restart_creates_durable_credential_rebind_and_restores_tool_arguments(tmp_path: Path) -> None:
    bearer = "BEARER_SECRET_1234567890"
    api_key = "sk-1234567890abcdefghijklmnop"
    root = tmp_path / "operations"
    first = OperationRuntime(root=root, register_builtins=False)
    state = first.start(
        session_id="credential-rebind",
        objective=f"Inspect https://target.invalid with Authorization: Bearer {bearer}; report results",
        starting_context={"nested": {"api_key": api_key}},
    )

    restarted = OperationRuntime(root=root, register_builtins=False)
    waiting = restarted.resume(state.run_id, max_actions=1)
    persisted_waiting = restarted.store.load_operation(state.run_id)

    assert waiting.state.status == "waiting_dependency"
    assert waiting.missing_capabilities == ("credential_rebind",)
    assert persisted_waiting is not None
    dependency = persisted_waiting.dependencies["credential_rebind"]
    assert dependency["status"] == "pending"
    assert set(dependency["required_refs"]) == set(persisted_waiting.credential_refs)
    assert dependency["durable_secret_material"] is False

    bindings = {
        secret_reference(bearer): bearer,
        secret_reference(api_key): api_key,
    }
    response = RuntimeMcpServer(restarted)._call_tool(
        "redteam_run",
        {
            "run_id": state.run_id,
            "credential_bindings": bindings,
            "auto_continue": False,
            "max_actions": 1,
        },
    )["structuredContent"]
    rebound = restarted.store.load_operation(state.run_id)

    assert rebound is not None
    assert "credential_rebind" not in rebound.dependencies
    assert "credential_rebind" not in response["dependencies"]
    descriptor = restarted.broker.register_adapter(
        name="credential-argument-capture",
        capabilities=("surface_mapping",),
        adapter=lambda arguments: arguments,
    )
    workflow = restarted._workflow_for(rebound)
    action = workflow.actions[0]
    arguments, _, _ = restarted.executor.arguments_for(rebound, workflow, action, descriptor)
    rendered_arguments = json.dumps(arguments, ensure_ascii=False, default=str)
    assert bearer in rendered_arguments
    assert api_key in rendered_arguments
    assert bearer not in json.dumps(response, ensure_ascii=False, default=str)
    assert api_key not in json.dumps(response, ensure_ascii=False, default=str)
    assert bearer not in restarted.store.path.read_bytes().decode("utf-8", errors="ignore")
    assert api_key not in restarted.store.path.read_bytes().decode("utf-8", errors="ignore")
    assert bearer not in repr(restarted._credential_vault)
    assert api_key not in repr(restarted._credential_vault)


def test_original_source_identity_prevents_same_shape_credential_collision(tmp_path: Path) -> None:
    first_secret = "TOKEN_VALUE_1234567890_A"
    second_secret = "TOKEN_VALUE_1234567890_B"
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    first = runtime.start(
        session_id="identity-collision",
        objective=f"Inspect https://target.invalid with Bearer {first_secret}",
    )
    second = runtime.start(
        session_id="identity-collision",
        objective=f"Inspect https://target.invalid with Bearer {second_secret}",
    )

    first_projection = first.goal.intent_envelope["input_redaction"]
    second_projection = second.goal.intent_envelope["input_redaction"]
    assert first_projection["original_bytes"] == second_projection["original_bytes"]
    assert first_projection["original_sha256"] != second_projection["original_sha256"]
    assert first.run_id != second.run_id
    database = runtime.store.path.read_bytes().decode("utf-8", errors="ignore")
    assert first_secret not in database
    assert second_secret not in database


def test_credential_vault_and_projection_do_not_expose_raw_values_in_repr() -> None:
    secret = "sk-1234567890abcdefghijklmnop"
    projected, bindings = project_sensitive({"nested": {"api_key": secret}})
    vault = CredentialVault()
    vault.bind_many(bindings)

    assert projected["nested"]["api_key"] == secret_reference(secret)
    assert secret not in repr(bindings)
    assert secret not in repr(vault)
    assert vault.resolve(projected)["nested"]["api_key"] == secret
    assert secret not in repr(ToolCallResult(status="success", output={"api_key": secret}))


def test_implicit_url_query_secret_keeps_complete_reference_in_target() -> None:
    secret = "sk-1234567890abcdefghijklmnop"
    reference = secret_reference(secret)
    goal = GoalCompiler().compile(
        f"Inspect https://target.invalid/api?api_key={secret}; report results"
    )

    assert goal.targets == (f"https://target.invalid/api?api_key={reference}",)
    assert reference in goal.intent_envelope["credential_refs"]


def test_url_basic_auth_projects_credentials_without_breaking_target() -> None:
    credentials = "alice:password-value-123456"
    reference = secret_reference(credentials)
    goal = GoalCompiler().compile(
        f"Inspect https://{credentials}@target.invalid/private and report"
    )

    assert goal.targets == (f"https://{reference}@target.invalid/private",)
    assert reference in goal.intent_envelope["credential_refs"]
    assert credentials not in json.dumps(goal.to_dict(), ensure_ascii=False)


def test_explicit_target_query_secret_uses_same_stable_reference_contract() -> None:
    secret = "sk-1234567890abcdefghijklmnop"
    reference = secret_reference(secret)
    goal = GoalCompiler().compile(
        "Inspect the supplied endpoint and report",
        targets=(f"https://target.invalid/api?api_key={secret}",),
    )

    assert goal.targets == (f"https://target.invalid/api?api_key={reference}",)
    assert goal.intent_envelope["targets"] == list(goal.targets)
    assert reference in goal.intent_envelope["credential_refs"]


def test_tool_echo_is_reprojected_before_cache_evidence_and_mcp_output(tmp_path: Path) -> None:
    secret = "sk-1234567890abcdefghijklmnop"
    reference = secret_reference(secret)
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="tool-echo-projection",
        objective=f"Inspect https://target.invalid/api?api_key={secret}; report results",
        starting_context={"nested": {"api_key": secret}},
    )
    workflow = runtime._workflow_for(state)
    action = workflow.actions[0]
    observed_arguments: dict[str, object] = {}

    def echo(arguments: dict[str, object]) -> dict[str, object]:
        observed_arguments.update(arguments)
        return {
            "target": arguments["target"],
            "assets": ["fixture-service"],
            "echo": {"api_key": secret},
        }

    runtime.broker.register_adapter(
        name="secret-echo",
        capabilities=action.required_capabilities,
        adapter=echo,
    )
    runtime.resume(state.run_id, max_actions=1)

    rendered_arguments = json.dumps(observed_arguments, ensure_ascii=False, default=str)
    evidence = runtime.store.evidence(state.run_id)
    rendered_evidence = json.dumps([node.to_dict() for node in evidence], ensure_ascii=False, default=str)
    rendered_mcp = json.dumps(
        RuntimeMcpServer(runtime)._call_tool(
            "redteam_status",
            {"run_id": state.run_id},
        )["structuredContent"],
        ensure_ascii=False,
        default=str,
    )
    database = runtime.store.path.read_bytes().decode("utf-8", errors="ignore")

    assert secret in rendered_arguments
    assert evidence
    assert secret not in rendered_evidence
    assert secret not in rendered_mcp
    assert secret not in database
    assert reference in rendered_evidence
    assert f"https://target.invalid/api?api_key={reference}" in rendered_evidence


def test_secret_reference_projection_is_idempotent_inside_url_and_nested_payload() -> None:
    secret = "sk-1234567890abcdefghijklmnop"
    reference = secret_reference(secret)
    value = {
        "url": f"https://target.invalid/api?api_key={reference}&token={reference}",
        "nested": {"api_key": reference, "authorization": reference},
    }

    projected, bindings = project_sensitive(value)
    projected_again, second_bindings = project_sensitive(projected)

    assert projected == value
    assert projected_again == value
    assert bindings == {}
    assert second_bindings == {}


def test_tampered_canonical_rewrite_with_redaction_still_fails_integrity(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "operations", register_builtins=False)
    state = runtime.start(
        session_id="redacted-rewrite-tamper",
        objective=(
            "Inspect https://target.invalid with Authorization: Bearer "
            "SUPER_SECRET_TOKEN_1234567890; write the report"
        ),
    )
    persisted = runtime.store.load_operation(state.run_id)
    assert persisted is not None
    envelope = dict(persisted.goal.intent_envelope)
    envelope["execution_prompt"] = str(envelope["execution_prompt"]).replace(
        "write the report",
        "omit the report",
    )
    persisted.goal = replace(persisted.goal, intent_envelope=envelope)
    runtime.store.save_operation(persisted, expected_version=persisted.state_version)

    observed = runtime.status(state.run_id)

    assert observed.state.status == "failed_integrity"
    assert observed.terminal.reason == "prompt_rewrite_contract_mismatch"
