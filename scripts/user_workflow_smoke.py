"""Authenticated workflow routes with isolated state and local evidence."""
from __future__ import annotations

import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from discussion_smoke import Stub
from reporting_smoke import persist


def main():
    with tempfile.TemporaryDirectory(prefix="trace-workflow-") as temporary, patch.dict(os.environ,
            {"TRACE_ADMIN_USERNAME": "trace", "TRACE_ADMIN_PASSWORD": "workflow-pass"}):
        with AgentService(root=Path(temporary), model_port=Stub(), load_external_configuration=False) as service:
            api = WebApi(service)
            headers = {"authorization": "Bearer " + api.control.login("trace", "workflow-pass")}
            run_id = service.start({"session_id": "workflow", "objective": "Explain local observations",
                "targets": [temporary]}).single.run.run_id

            def request(method, route, body=None, command=""):
                result = api.dispatch(method, route, body=body,
                    headers={**headers, **({"x-command-id": command} if command else {})})
                assert result.status == 200, (route, result.status, result.body)
                return result

            for index in range(450):
                service.conversation.append(run_id=run_id, role="assistant" if index % 2 else "user",
                    content=f"text-{index}" if index % 2 else {"text": f"text-{index}"},
                    protected=False, source_type="fixture", source_id=str(index))
            service.conversation.append(run_id=run_id, role="user", content="projection-duplicate",
                protected=False, source_type="model_request_projection", source_id="excluded")
            route = f"/api/conversations/{run_id}/entries"
            assert api.dispatch("GET", route).status == 401
            with patch.object(service, "export_session", side_effect=AssertionError("full journal export")), \
                 patch.object(service, "session_tree", side_effect=AssertionError("full tree read")):
                first = request("GET", route + "?limit=200").payload()
                second = request("GET", route + "?limit=200&offset=200").payload()
                last = request("GET", route + "?limit=200&offset=400").payload()
            assert [len(page["entries"]) for page in (first, second, last)] == [200, 200, 51]
            assert first["next_offset"] == 200 and second["next_offset"] == 400 and not last["truncated"]
            entries = first["entries"] + second["entries"] + last["entries"]
            assert len({item["entry_id"] for item in entries}) == 451
            assert first["entries"][0]["role"] == "assistant" and first["entries"][0]["text"] == "text-449"
            assert all(item["created_at"] for item in entries)
            assert all("projection-duplicate" not in item["text"] for item in entries)

            state = service.runtime.store.load_operation(run_id)
            node = persist(service.runtime, state, "observed", "surface_map",
                {"assets": [temporary], "body": "local body <script>not executable</script>"})
            child = persist(service.runtime, state, "child", "surface_map",
                {"assets": [temporary], "body": "child body"}, parents=(node.evidence_id,))
            graph = request("GET", f"/api/runs/{run_id}/evidence-graph?include_payload=1").payload()
            assert all("payload" in item for item in graph["nodes"])
            detail = request("GET", f"/api/runs/{run_id}/evidence-graph/{node.evidence_id}").payload()
            assert detail["node"]["payload"]["body"].startswith("local body")
            assert [item["evidence_id"] for item in detail["descendants"]] == [child.evidence_id]
            missing = api.dispatch("GET", f"/api/runs/{run_id}/evidence-graph/missing", headers=headers)
            assert missing.status == 404
            for format, media in (("markdown", "text/markdown"), ("html", "text/html")):
                response = request("GET", f"/api/runs/{run_id}/report?format={format}")
                assert response.content_type.startswith(media)
                assert "attachment" in response.headers["Content-Disposition"]
                assert "local body" in response.body.decode("utf-8")
                if format == "html":
                    assert "&lt;script&gt;not executable&lt;/script&gt;" in response.body.decode("utf-8")
            report = request("GET", f"/api/runs/{run_id}/report").payload()
            assert not report["findings"] and not report["terminal"]["success"]

            service.cancel(run_id, "fixture-finished")
            before = service.status(run_id).run.to_dict()
            route = f"/api/runs/{run_id}/discussion"
            result = request("POST", route, {"text": "Explain the result"}, "discussion-once").payload()
            assert result["response"]["status"] == "completed"
            assert request("POST", route, {"text": "Explain the result"}, "discussion-once").payload() == result
            assert service.status(run_id).run.to_dict() == before
            transparency = request("GET", f"/api/runs/{run_id}/transparency").payload()
            assert transparency["model"]["discussion_totals"]["total_tokens"] == 12
            assert transparency["model"]["totals"]["requests"] == 0
            with patch.object(api.control, "test_provider", return_value={"ok": False,
                    "reply": "", "model": "fixture", "latency_ms": 1, "error": "fixture_error"}):
                result = request("POST", "/api/providers/test", {"model": "fixture"}, "provider-test").payload()
            assert result["ok"] and not result["success"] and result["error"] == "fixture_error"
    print("user_workflow_smoke: PASS (auth, entries, text roles, paging, evidence, reports, discussion replay, usage, provider result)")


if __name__ == "__main__":
    main()
