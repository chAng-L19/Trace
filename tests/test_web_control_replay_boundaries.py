from pathlib import Path

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi


def test_legacy_provider_switch_can_return_to_previous_selection(tmp_path: Path):
    service = AgentService(root=tmp_path / "runtime")
    try:
        api = WebApi(service)
        for name in ("one", "two"):
            assert api.dispatch("POST", "/api/providers", body={
                "provider_id": name, "name": name,
                "base_url": "http://127.0.0.1:9/v1", "model": name,
            }).status == 201
        for name in ("one", "two", "one"):
            response = api.dispatch("POST", "/api/providers/active", body={"provider_id": name})
            assert response.status == 200
            assert service.model_loop.model_name == name
            replay = api.dispatch("POST", "/api/providers/active", body={"provider_id": name},
                                  headers={"X-Command-ID": response.headers["X-Command-ID"]})
            assert replay.body == response.body
    finally:
        service.close()


def test_control_command_key_cannot_replay_different_http_method(tmp_path: Path):
    service = AgentService(root=tmp_path / "runtime")
    try:
        api = WebApi(service)
        body = {"provider_id": "fixture", "name": "fixture", "model": "one",
                "base_url": "http://127.0.0.1:9/v1"}
        assert api.dispatch("POST", "/api/providers", body=body).status == 201
        first = api.dispatch("POST", "/api/providers/fixture", headers={"X-Command-ID": "same-method-key"})
        assert first.status == 404
        conflicting = api.dispatch("DELETE", "/api/providers/fixture", headers={"X-Command-ID": "same-method-key"})
        assert conflicting.status == 409
        assert api.dispatch("GET", "/api/providers/fixture").status == 200
    finally:
        service.close()
