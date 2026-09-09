from __future__ import annotations

import json
from pathlib import Path

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import ModelResponse, ToolCall, ToolDefinition, ToolResult
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime.tool_registry import ToolCatalog, ToolRegistry, ToolVisibilityPolicy


class _Tools:
    def __init__(self) -> None:
        self.calls: list[str] = []
        self.items = (
            ToolDefinition(
                qualified_name="fixture:read",
                name="read",
                server="fixture",
                description="Read fixture data",
                capabilities=("page_fetch",),
                side_effecting=False,
            ),
            ToolDefinition(
                qualified_name="fixture:write",
                name="write",
                server="fixture",
                description="Change fixture data",
                capabilities=("controlled_validation",),
                side_effecting=True,
            ),
            ToolDefinition(
                qualified_name="fixture:secret",
                name="secret",
                server="fixture",
                description="Hidden capability",
                capabilities=("binary_reverse",),
                side_effecting=True,
            ),
        )

    def discover(self):
        return self.items

    def invoke(self, call: ToolCall) -> ToolResult:
        self.calls.append(call.tool_name)
        return ToolResult(call_id=call.call_id, status="success", tool_name=call.tool_name, output={"ok": True})

    def reconcile(self, call: ToolCall):
        return None

    def cancel(self, call_id: str) -> bool:
        return True


def _run(service: AgentService) -> str:
    return service.start(
        StartRequest(session_id="registry", objective="Give me a plan; do not make changes yet")
    ).single.run.run_id


def test_default_catalog_hides_side_effecting_tools_and_expand_restores_them(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime", tool_port=_Tools())
    run_id = _run(service)

    default = service.tool_catalog(run_id, capabilities=("page_fetch",))

    assert {item.qualified_name for item in default.tools} == {"fixture:read"}
    assert default.expanded is False
    assert any(item.reason == "deferred_until_expand" for item in default.visibility)
    assert default.estimated_prompt_bytes > 0

    expanded = service.expand_tools(run_id, ("fixture:write",))

    assert {item.qualified_name for item in expanded.tools} == {"fixture:write"}
    assert expanded.expanded is True
    assert (
        next(item for item in expanded.visibility if item.qualified_name == "fixture:write").reason
        == "explicit_expand"
    )


def test_registry_rejects_hidden_calls_and_accepts_expanded_calls(tmp_path: Path) -> None:
    delegate = _Tools()
    service = AgentService(root=tmp_path / "runtime", tool_port=delegate)
    run_id = _run(service)
    hidden = ToolCall(call_id="hidden", run_id=run_id, tool_name="fixture:write", arguments={})

    result = service.tools.invoke(hidden)

    assert result.status == "failed"
    assert result.error == "tool_not_visible:expand_required"
    assert delegate.calls == []

    expanded = service.expand_tools(run_id)
    assert {item.qualified_name for item in expanded.tools} == {
        "fixture:read",
        "fixture:write",
        "fixture:secret",
    }
    result = service.tools.invoke(hidden)

    assert result.status == "success"
    assert delegate.calls == ["fixture:write"]


def test_capability_selected_side_effect_tool_remains_allowed_for_run_and_restart(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    first_delegate = _Tools()
    first = AgentService(root=root, tool_port=first_delegate)
    run_id = _run(first)
    first.tool_catalog(run_id, capabilities=("controlled_validation",))
    call = ToolCall(call_id="selected", run_id=run_id, tool_name="fixture:write", arguments={})

    assert first.tools.invoke(call).status == "success"

    recovered_delegate = _Tools()
    recovered = AgentService(root=root, tool_port=recovered_delegate)
    assert recovered.tools.invoke(call).status == "success"
    assert recovered_delegate.calls == ["fixture:write"]


def test_selected_tool_metadata_change_requires_catalog_refresh(tmp_path: Path) -> None:
    delegate = _Tools()
    service = AgentService(root=tmp_path / "runtime", tool_port=delegate)
    run_id = _run(service)
    service.tool_catalog(run_id, capabilities=("page_fetch",))
    original = delegate.items[0]
    delegate.items = (
        ToolDefinition(
            qualified_name=original.qualified_name,
            name=original.name,
            server=original.server,
            description=original.description,
            capabilities=original.capabilities,
            side_effecting=True,
        ),
        *delegate.items[1:],
    )
    call = ToolCall(call_id="changed", run_id=run_id, tool_name="fixture:read", arguments={})

    result = service.tools.invoke(call)

    assert result.status == "failed"
    assert result.error == "tool_catalog_changed:refresh_required"
    assert delegate.calls == []


def test_unknown_tool_and_model_side_effect_override_are_rejected(tmp_path: Path) -> None:
    delegate = _Tools()
    service = AgentService(root=tmp_path / "runtime", tool_port=delegate)
    run_id = _run(service)

    unknown = service.tools.invoke(
        ToolCall(call_id="unknown", run_id=run_id, tool_name="fixture:missing", arguments={})
    )
    override = service.tools.invoke(
        ToolCall(
            call_id="override",
            run_id=run_id,
            tool_name="fixture:write",
            arguments={},
            metadata={"side_effecting": False},
        )
    )

    assert unknown.error == "tool_not_visible:expand_required"
    assert override.error == "tool_not_visible:expand_required"
    assert delegate.calls == []


def test_expansion_survives_service_restart_and_catalog_revision_is_stable(tmp_path: Path) -> None:
    root = tmp_path / "runtime"
    first = AgentService(root=root, tool_port=_Tools())
    run_id = _run(first)
    expanded = first.expand_tools(run_id, ("fixture:secret",))

    recovered = AgentService(root=root, tool_port=_Tools())
    replayed = recovered.tool_catalog(run_id)

    assert replayed.expanded is True
    assert {item.qualified_name for item in replayed.tools} == {"fixture:secret"}
    assert replayed.revision == expanded.revision


def test_profiles_add_capabilities_without_exposing_all_tools() -> None:
    policy = ToolVisibilityPolicy()
    tools = _Tools().items

    selected, _ = policy.select(tools, capabilities=policy.profile_capabilities("binary"))

    assert {item.qualified_name for item in selected} == {"fixture:read", "fixture:secret"}
    assert "fixture:write" not in {item.qualified_name for item in selected}


def test_default_catalog_reduces_prompt_tool_payload() -> None:
    class ManyTools(_Tools):
        def __init__(self) -> None:
            self.items = tuple(
                ToolDefinition(
                    qualified_name=f"mcp:tool-{index}",
                    name=f"tool-{index}",
                    server="mcp",
                    description=("A long tool description " * 10) + "\nOmitted detail",
                    capabilities=("page_fetch",) if index < 2 else ("active",),
                    side_effecting=index >= 2,
                    metadata={"source": "live-mcp"},
                    input_schema={"type": "object", "properties": {"value": {"type": "string"}}},
                )
                for index in range(10)
            )

    registry = ToolRegistry(ManyTools())
    all_tools, _ = registry.policy.select(registry.delegate.discover(), expanded=True)
    full = ToolRegistry.prompt_definitions(ToolCatalog("full", all_tools, (), True, 0))
    selected = registry.catalog(capabilities=("page_fetch",))
    compact = ToolRegistry.prompt_definitions(selected)

    full_bytes = len(json.dumps(full, ensure_ascii=False))
    compact_bytes = len(json.dumps(compact, ensure_ascii=False))
    assert compact_bytes <= full_bytes * 0.7
    assert len(selected.tools) == 2
    assert all("\n" not in item["description"] and len(item["description"]) <= 240 for item in compact)


def test_model_can_explicitly_expand_then_call_hidden_tool(tmp_path: Path) -> None:
    provider = FakeModelProvider(
        [
            ModelResponse(
                request_id="placeholder",
                status="completed",
                structured_output={"tools_expand": ["fixture:write"]},
                finish_reason="stop",
            ),
            ModelResponse(
                request_id="placeholder",
                status="completed",
                tool_calls=({"id": "expanded-call", "name": "fixture:write", "arguments": {}},),
                finish_reason="tool_calls",
            ),
        ]
    )
    service = AgentService(root=tmp_path / "runtime", model_port=provider, tool_port=_Tools(), model_max_turns=2)
    target = tmp_path / "target.txt"
    target.write_text("fixture\n", encoding="utf-8")
    run_id = service.start(
        StartRequest(
            session_id="registry-model",
            objective=(
                f"Inspect {target}; validate the highest-value path; prove impact; "
                "run a negative control; verify cleanup; write the final report"
            ),
            targets=(str(target),),
        )
    ).single.run.run_id

    service.run(run_id)

    assert provider.requests[0].metadata["tool_catalog_expanded"] is False
    assert provider.requests[1].metadata["tool_catalog_expanded"] is True


def test_catalog_revision_changes_when_delegate_tool_list_changes(tmp_path: Path) -> None:
    delegate = _Tools()
    service = AgentService(root=tmp_path / "runtime", tool_port=delegate)
    run_id = _run(service)
    first = service.tool_catalog(run_id)
    delegate.items = (*delegate.items, ToolDefinition(
        qualified_name="fixture:new",
        name="new",
        server="fixture",
        description="New read-only tool",
        capabilities=("page_fetch",),
    ))

    second = service.tool_catalog(run_id, capabilities=("page_fetch",))

    assert second.revision != first.revision
    assert "fixture:new" in {item.qualified_name for item in second.tools}
    selected_events = [
        item for item in service.events(run_id) if item.event_type == "tool_catalog_selected"
    ]
    assert len(selected_events) >= 2
    payload = selected_events[-1].payload
    selected_tool = next(item for item in payload["selected_tools"] if item["name"] == "fixture:new")
    assert payload["revision"] == second.revision
    assert selected_tool["schema_hash"] and "source" in selected_tool
    assert {item["reason"] for item in payload["visibility"]} >= {
        "capability_match",
        "deferred_until_expand",
    }
