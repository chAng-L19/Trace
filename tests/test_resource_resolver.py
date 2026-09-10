from __future__ import annotations

from pathlib import Path

from redteam_agent import AgentService, StartRequest
from redteam_agent.application import ResourceResolver
from redteam_agent.core import ModelCapabilities, ModelResponse
from redteam_agent.providers import FakeModelProvider


def _fixture(root: Path) -> None:
    (root / "skills" / "trace").mkdir(parents=True)
    (root / "mcp-instructions").mkdir()
    (root / "AGENTS.md").write_text("Runtime owns scope and evidence.\n", encoding="utf-8")
    (root / "context.md").write_text("Project context.\n", encoding="utf-8")
    (root / "skills" / "trace" / "SKILL.md").write_text(
        "Use the selected Trace capability only for this run.\n", encoding="utf-8"
    )
    (root / "mcp-instructions" / "browser.md").write_text(
        "Browser instructions are optional.\n", encoding="utf-8"
    )


def test_resource_index_and_default_selection_are_deterministic(tmp_path: Path) -> None:
    root = tmp_path / "resources"
    _fixture(root)
    resolver = ResourceResolver()

    first = resolver.index((root,))
    second = resolver.index((root,))
    selected = resolver.select(first)

    assert first.index_hash == second.index_hash
    assert [item.to_dict() for item in first.resources] == [item.to_dict() for item in second.resources]
    assert {item.kind for item in selected.selected} == {"agents", "project_context"}
    assert not any(item.kind in {"skill", "mcp_instruction"} for item in selected.selected)
    assert all(item.content_hash for item in first.resources)


def test_resource_selection_honors_explicit_patterns_disable_and_budget(tmp_path: Path) -> None:
    root = tmp_path / "resources"
    _fixture(root)
    resolver = ResourceResolver(max_resource_tokens=100)
    index = resolver.index((root,))

    selected = resolver.select(
        index,
        requested=("skill:*", "mcp_instruction:*"),
        disabled=("mcp_instruction:*",),
        token_budget=20,
    )

    assert all(item.kind == "skill" for item in selected.selected)
    assert selected.disabled
    assert selected.token_cost <= 20
    assert selected.selection_hash
    assert selected.prompt_projection()["resources"][0]["content"]


def test_resource_roots_and_selection_are_visible_in_model_request_without_evidence_promotion(
    tmp_path: Path,
) -> None:
    root = tmp_path / "resources"
    _fixture(root)
    capabilities = ModelCapabilities(
        native_system_role=True,
        native_tool_calls=True,
        structured_output=True,
        usage_reporting=True,
        max_context_tokens=4096,
        metadata={"provider": "fake", "model": "resource-fixture"},
    )
    provider = FakeModelProvider(
        [ModelResponse(request_id="placeholder", status="completed", text="resource reviewed")],
        capabilities=capabilities,
    )
    service = AgentService(root=tmp_path / "runtime", model_port=provider)
    run_id = service.start(
        StartRequest(
            session_id="resource-selection",
            objective="Review the supplied project context",
            constraints={
                "resource_roots": [str(root)],
                "resources": ["agents:*", "skill:*"],
            },
        )
    ).single.run.run_id

    service.run(run_id)

    request = provider.requests[0]
    metadata = request.metadata
    assert metadata["resource_index_hash"]
    assert metadata["resource_selection_hash"]
    assert metadata["resource_ids"]
    assert metadata["resource_tokens"] > 0
    assert any(
        message.get("content", {}).get("resource_context")
        for message in request.messages
        if isinstance(message.get("content"), dict)
    )
    assert service.status(run_id).evidence == ()
    assert service.status(run_id).goal.objective == "Review the supplied project context"


def test_symlink_resource_root_is_reported_without_traversal(tmp_path: Path) -> None:
    target = tmp_path / "target"
    target.mkdir()
    (target / "AGENTS.md").write_text("do not traverse through a link\n", encoding="utf-8")
    linked = tmp_path / "linked"
    try:
        linked.symlink_to(target, target_is_directory=True)
    except (OSError, NotImplementedError):
        return

    index = ResourceResolver().index((linked,))

    assert index.resources == ()
    assert len(index.issues) == 1
    assert index.issues[0].error == "resource_root_symlink"
