"""Run-bound resource discovery/loading and credential-schema regression checks."""
from __future__ import annotations

import json
import sys
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import ToolCall, contract_hash
from redteam_agent.runtime.security import project_sensitive, redact_sensitive


def invoke(service, run_id, name, arguments):
    return service.tools.invoke(ToolCall(call_id=contract_hash({"name": name, "args": arguments})[:20],
        run_id=run_id, tool_name="agent:" + name, arguments=arguments,
        idempotency_key=contract_hash({"run": run_id, "name": name, "args": arguments})))


def schema_boundaries():
    secret = "fixture-sensitive-schema-value"
    cases = [{"parameters": {container: {"password": secret}}}
             for container in ("properties", "$defs", "definitions", "patternProperties")]
    cases += [{"input_schema": {"properties": {"password": {field: secret}}}}
              for field in ("description", "unknown", "type", "minLength", "minimum", "readOnly", "required", "$ref", "format")]
    cases += [{"input_schema": {"properties": {"password": {field: [secret]}}}}
              for field in ("allOf", "anyOf", "oneOf", "prefixItems")]
    cases.append({"input_schema": {"properties": {"password": {
        "properties": {"value": {"default": secret}}}}}})
    for case in cases:
        projected, bindings = project_sensitive(case)
        assert bindings and secret not in json.dumps(projected), case
        assert secret not in json.dumps(redact_sensitive(case)), case
        assert project_sensitive(projected)[0] == projected, case
        assert redact_sensitive(projected) == projected, case
    valid = {"input_schema": {"type": "object", "properties": {
        "password": {"type": "object", "additionalProperties": False,
                     "properties": {"value": {"type": "string", "minLength": 3}}, "required": ["value"]},
        "credential_ref": False}}}
    assert project_sensitive(valid)[0] == valid
    assert redact_sensitive(valid) == valid


def resource_boundaries():
    with tempfile.TemporaryDirectory(prefix="trace-resource-collision-") as temporary:
        root = Path(temporary)
        roots = [root / name for name in ("a", "b")]
        for directory in roots:
            path = directory / "skills" / "fixture" / "SKILL.md"
            path.parent.mkdir(parents=True)
            path.write_text(str(directory), encoding="utf-8")
        with AgentService(root=root / "runtime", load_external_configuration=False) as service:
            run_id = service.start(StartRequest(session_id="collision", objective="Inspect fixture",
                targets=("fixture.invalid",), constraints={"resource_roots": [str(item) for item in roots]})).single.run.run_id
            index = service.resource_index(run_id)
            assert sum(item.error.startswith("resource_id_ambiguous:") for item in index.issues) == 2
            assert not any("fixture" in item.resource_id for item in index.resources)
            assert invoke(service, run_id, "load_resource", {"resource_id": "skill:skills/fixture/SKILL.md"}).status == "failed"

    with tempfile.TemporaryDirectory(prefix="trace-resource-budget-") as temporary:
        root = Path(temporary)
        for name in ("a", "b"):
            path = root / "skills" / name / "SKILL.md"
            path.parent.mkdir(parents=True)
            path.write_text(name * 10000, encoding="utf-8")  # 2500 tokens each, combined >4096.
        with AgentService(root=root, load_external_configuration=False) as service:
            run_id = service.start(StartRequest(session_id="budget", objective="Inspect fixture", targets=("fixture.invalid",))).single.run.run_id
            ids = [item.resource_id for item in service.resource_index(run_id).resources
                   if "/a/" in item.resource_id or "/b/" in item.resource_id]
            assert invoke(service, run_id, "load_resource", {"resource_id": ids[1]}).status == "success"
            assert invoke(service, run_id, "load_resource", {"resource_id": ids[0]}).status == "failed"
            assert service.resource_selection(run_id).resource_ids == (ids[1],)
            other_run = service.start(StartRequest(session_id="concurrent", objective="Inspect fixture", targets=("fixture.invalid",))).single.run.run_id
            with AgentService(root=root, load_external_configuration=False) as other_service:
                service.tool_catalog(other_run)
                other_service.tool_catalog(other_run)
                barrier = threading.Barrier(2)

                def delayed_append(original):
                    def append(*args, **kwargs):
                        if args[1] == "resource_loaded":
                            try:
                                barrier.wait(timeout=0.3)
                            except threading.BrokenBarrierError:
                                pass  # An atomic writer prevents the second check reaching this point.
                        return original(*args, **kwargs)
                    return append

                with patch.object(service.runtime.store, "append_event_once", side_effect=delayed_append(service.runtime.store.append_event_once)), \
                     patch.object(other_service.runtime.store, "append_event_once", side_effect=delayed_append(other_service.runtime.store.append_event_once)):
                    with ThreadPoolExecutor(max_workers=2) as pool:
                        futures = [pool.submit(invoke, selected_service, other_run, "load_resource", {"resource_id": identity})
                                   for selected_service, identity in zip((service, other_service), ids)]
                        results = [future.result(timeout=10) for future in futures]
                assert sorted(result.status for result in results) == ["failed", "success"]
                assert len(service.resource_selection(other_run).resource_ids) == 1


def main() -> None:
    schema_boundaries()
    schema = {"type": "object", "properties": {"credential_ref": {"type": "string"},
        "token_env": {"type": "string"}, "password": {"type": "string", "default": "do-not-persist"}},
        "required": ["credential_ref"]}
    redacted = redact_sensitive({"input_schema": schema})["input_schema"]
    assert redacted["properties"]["credential_ref"] == {"type": "string"}
    assert redacted["properties"]["token_env"] == {"type": "string"}
    assert "do-not-persist" not in json.dumps(redacted)
    projected, bindings = project_sensitive({"input_schema": schema})
    assert projected["input_schema"]["properties"]["credential_ref"] == {"type": "string"}
    assert bindings and "do-not-persist" not in json.dumps(projected)
    assert project_sensitive(projected)[0] == projected
    assert redact_sensitive(projected) == projected
    assert redact_sensitive({"input_schema": {"properties": {"password": {"default": "leak"}}}})["input_schema"]["properties"]["password"]["default"] != "leak"
    assert redact_sensitive({"parameters": {"password": "actual-secret"}})["parameters"]["password"] != "actual-secret"
    assert redact_sensitive({"password": {"type": "string", "default": "secret"}})["password"] != {"type": "string", "default": "secret"}

    with tempfile.TemporaryDirectory(prefix="trace-resource-") as temporary:
        root = Path(temporary)
        guidance = root / "AGENTS.md"
        guidance.write_text("Preserve the task's evidence.", encoding="utf-8")
        procedure = root / "skills" / "fixture" / "SKILL.md"
        procedure.parent.mkdir(parents=True)
        procedure.write_text("Fixture procedure version 1.", encoding="utf-8")
        service = AgentService(root=root, load_external_configuration=False)
        run_id = service.start(StartRequest(session_id="resources", objective="Assess fixture://resources",
                                            targets=("fixture://resources",))).single.run.run_id
        other = service.start(StartRequest(session_id="other", objective="Assess fixture://other",
                                           targets=("fixture://other",))).single.run.run_id
        initial = service.resource_selection(run_id)
        assert any(item.kind == "agents" for item in initial.selected)
        assert not any(item.kind == "skill" for item in initial.selected)
        listed = invoke(service, run_id, "list_resources", {})
        assert listed.status == "success", listed.error
        builtin = next(item for item in listed.output["items"] if "api-recon" in item["resource_id"])
        fixture_id = next(item.resource_id for item in service.resource_index(run_id).resources
                          if item.source == str(procedure.resolve()))
        loaded = invoke(service, run_id, "load_resource", {"resource_id": fixture_id})
        assert loaded.status == "success" and loaded.output["authority"] == "guidance_only", loaded.error
        selected = service.resource_selection(run_id)
        assert fixture_id in selected.resource_ids and any(item.kind == "agents" for item in selected.selected)
        assert fixture_id not in service.resource_selection(other).resource_ids
        assert invoke(service, run_id, "load_resource", {"resource_id": fixture_id}).status == "success"
        service.close()
        service = AgentService(root=root, load_external_configuration=False)
        assert fixture_id in service.resource_selection(run_id).resource_ids
        procedure.write_text("Fixture procedure version 2.", encoding="utf-8")
        assert fixture_id not in service.resource_selection(run_id).resource_ids
        assert invoke(service, run_id, "load_resource", {"resource_id": fixture_id}).status == "success"
        service.control.set_skill(fixture_id, enabled=False)
        assert invoke(service, run_id, "load_resource", {"resource_id": fixture_id}).status == "failed"
        assert fixture_id not in service.resource_selection(run_id).resource_ids
        assert invoke(service, run_id, "load_resource", {"resource_id": "missing"}).status == "failed"
        assert invoke(service, run_id, "load_resource", {"resource_id": builtin["resource_id"], "run_id": other}).status == "failed"
        assert invoke(service, run_id, "load_resource", {"resource_id": builtin["resource_id"]}).status == "success"
        service.control.set_skill(builtin["resource_id"], enabled=True)
        assert any(item.kind == "agents" for item in service.resource_selection(run_id).selected)
        assert not service.runtime.evidence_graph.list(run_id)
        service.close()
    resource_boundaries()
    print(json.dumps({"ok": True, "checks": ["schema-definitions", "schema-secret-defaults", "query-secrets",
        "builtin-discovery", "on-demand-loading", "default-guidance-preserved", "run-isolation",
        "durable-resume", "changed-source-requires-reload", "disabled-resource", "no-evidence-promotion",
        "malformed-schema-secrets", "schema-composition", "enabled-skill-preserves-guidance",
        "ambiguous-resource-rejected", "budget-no-eviction", "atomic-concurrent-load"]}))


if __name__ == "__main__":
    main()
