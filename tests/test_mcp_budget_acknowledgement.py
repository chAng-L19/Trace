from pathlib import Path

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import ModelResponse
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime.mcp_server import RuntimeMcpServer


def test_public_mcp_can_acknowledge_unknown_token_usage(tmp_path: Path) -> None:
    provider = FakeModelProvider([
        ModelResponse(request_id="placeholder", status="completed", text="usage absent"),
        ModelResponse(request_id="placeholder", status="completed", text="resumed",
                      usage={"total_tokens": 2}),
    ])
    service = AgentService(root=tmp_path / "runtime", model_port=provider)
    try:
        run_id = service.start(StartRequest(
            session_id="mcp-budget-ack", objective="Inspect supplied target", token_limit=10,
        )).single.run.run_id
        paused = service.run(run_id)
        assert paused.run.budget.pause_reason == "token_usage_unknown"
        assert service.runtime.store.load_operation(run_id).budget.tokens_used is None
        server = RuntimeMcpServer(service.runtime, service=service)
        response = server.handle({
            "jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": "redteam_run", "arguments": {
                "run_id": run_id, "auto_continue": False,
                "budget_delta": {"acknowledge_missing_usage": True},
            }},
        })
        assert "error" not in response, response
        assert len(provider.requests) == 2
        budget = service.runtime.store.load_operation(run_id).budget
        assert budget.token_usage_acknowledged == budget.token_usage_missing == 1
        assert budget.tokens_used == 2
    finally:
        service.close()
