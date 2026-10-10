"""Operator messages are durable user context, not tool observations."""
import json
import os
import tempfile
from pathlib import Path
from unittest.mock import patch
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
from redteam_agent import AgentService, StartRequest
from redteam_agent.adapters.web import WebApi


def main():
    with tempfile.TemporaryDirectory() as temporary, patch.dict(os.environ,
            {"TRACE_ADMIN_USERNAME": "trace", "TRACE_ADMIN_PASSWORD": "message-pass"}):
        root = Path(temporary)
        service = AgentService(root=root, load_external_configuration=False)
        try:
            run_id = service.start(StartRequest(session_id="messages", objective="Inspect fixture",
                targets=("fixture.invalid",))).single.run.run_id
            api = WebApi(service)
            headers = {"authorization": "Bearer " + api.control.login("trace", "message-pass"),
                       "x-command-id": "first-message"}
            route = f"/api/runs/{run_id}/message"
            assert api.dispatch("POST", route, body={"text": "message"}).status == 401
            body = {"text": "先检查登录接口，再验证会话隔离。"}
            first = api.dispatch("POST", route, body=body, headers=headers)
            assert first.status == 200, first.payload()
            assert first.payload()["message"]["role"] == "user"
            assert api.dispatch("POST", route, body=body, headers=headers).payload() == first.payload()
            messages = service.transcript(run_id)
            assert sum(item.source_type == "operator_message" for item in messages) == 1
            assert any(body["text"] in str(item.get("content")) for item in service.select_context(run_id).messages)
            assert api.dispatch("POST", route, body={"text": "different"}, headers=headers).status == 409
            assert api.dispatch("POST", route, body={"text": "  "},
                headers={**headers, "x-command-id": "empty-message"}).status == 400
            service.pause(run_id, "user_requested")
            assert api.dispatch("POST", route, body={"text": "补充上下文"},
                headers={**headers, "x-command-id": "paused-message"}).status == 200
            assert service.status(run_id).run.status == "paused_budget"
            service.cancel(run_id, "test-complete")
            assert api.dispatch("POST", route, body={"text": "ended"},
                headers={**headers, "x-command-id": "ended-message"}).status == 400
        finally:
            service.close()
        reopened = AgentService(root=root, load_external_configuration=False)
        try:
            assert sum(item.source_type == "operator_message" for item in reopened.transcript(run_id)) == 2
        finally:
            reopened.close()
    print(json.dumps({"ok": True, "checks": ["auth", "message-context", "durable-replay",
          "conflicting-command", "empty-text", "paused-input", "terminal-input", "restart"]}))


if __name__ == "__main__":
    main()
