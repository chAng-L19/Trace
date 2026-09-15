from __future__ import annotations

import json
from contextlib import contextmanager
from http.client import HTTPConnection
from pathlib import Path
from threading import Thread

import pytest

from redteam_agent import AgentService
from redteam_agent.adapters.web import (
    TraceHTTPServer,
    WebApi,
    model_provider_from_environment,
    serve,
)
from redteam_agent.providers import FakeModelProvider, OpenAICompatibleProvider


@contextmanager
def _server(tmp_path: Path, *, model: bool = False):
    provider = FakeModelProvider([]) if model else None
    service = AgentService(
        root=tmp_path / "runtime",
        model_port=provider,
        model_name="fixture-model" if provider else "",
    )
    server = TraceHTTPServer(("127.0.0.1", 0), WebApi(service))
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server, service
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
        service.close()


def _get(server: TraceHTTPServer, path: str):
    connection = HTTPConnection(*server.server_address, timeout=3)
    connection.request("GET", path)
    response = connection.getresponse()
    body = response.read()
    headers = dict(response.getheaders())
    connection.close()
    return response.status, headers, body


def _request(server: TraceHTTPServer, method: str, path: str, body: bytes = b""):
    connection = HTTPConnection(*server.server_address, timeout=3)
    headers = {"Content-Type": "application/json"} if body else {}
    connection.request(method, path, body=body or None, headers=headers)
    response = connection.getresponse()
    payload = response.read()
    connection.close()
    return response.status, payload


@pytest.mark.parametrize(
    ("path", "content_type", "needle"),
    [
        ("/", "text/html", b"Trace Workbench"),
        ("/app.css", "text/css", b"--green"),
        ("/app.js", "text/javascript", b"/api/runs"),
    ],
)
def test_workbench_static_assets_have_security_headers(
    tmp_path: Path, path: str, content_type: str, needle: bytes
) -> None:
    with _server(tmp_path) as (server, _):
        status, headers, body = _get(server, path)

    assert status == 200
    assert headers["Content-Type"].startswith(content_type)
    assert headers["X-Content-Type-Options"] == "nosniff"
    assert headers["X-Frame-Options"] == "DENY"
    assert "frame-ancestors 'none'" in headers["Content-Security-Policy"]
    assert needle in body


def test_system_endpoint_reports_model_without_secrets(tmp_path: Path) -> None:
    with _server(tmp_path, model=True) as (server, _):
        status, _, body = _get(server, "/api/system")

    payload = json.loads(body)
    assert status == 200
    assert payload["service"] == "Trace"
    assert payload["provider"]["configured"] is True
    assert payload["provider"]["name"] == "fake"
    assert payload["provider"]["model"] == "fixture-model"
    assert "metadata" not in payload["provider"]["capabilities"]
    assert "api_key" not in body.decode().casefold()


def test_http_delete_reaches_control_plane(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, _):
        status, _ = _request(
            server,
            "POST",
            "/api/mcp",
            json.dumps({"server_id": "fixture", "transport": "http", "url": "http://127.0.0.1:1"}).encode(),
        )
        assert status == 201
        status, payload = _request(server, "DELETE", "/api/mcp/fixture")
    assert status == 200
    assert json.loads(payload)["deleted"] == "fixture"


def test_conversation_projection_includes_active_branch(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = service.start({"session_id": "conversation-http", "objective": "Inspect branch state"}).single.run.run_id
        status, payload = _request(server, "GET", f"/api/conversations/{run_id}")

    assert status == 200
    response = json.loads(payload)
    assert response["session"]["run_id"] == run_id
    assert response["session"]["active_branch_id"] == "main"
    assert "main" in response["session"]["branches"]
    assert set(response["tree"]["nodes"]) >= set(response["session"]["branches"].values()) - {None}


def test_conversation_branch_and_checkout_routes_use_journal(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = service.start({"session_id": "conversation-branch", "objective": "Inspect branch state"}).single.run.run_id
        status, payload = _request(server, "GET", f"/api/conversations/{run_id}")
        assert status == 200
        tree = json.loads(payload)["tree"]["nodes"]
        entry_id = next(iter(tree))
        status, payload = _request(
            server, "POST", f"/api/conversations/{run_id}/fork",
            json.dumps({"from_entry_id": entry_id, "branch_id": "review"}).encode(),
        )
        assert status == 200
        status, payload = _request(
            server, "POST", f"/api/conversations/{run_id}/checkout",
            json.dumps({"branch_id": "main"}).encode(),
        )
    assert status == 200
    response = json.loads(payload)
    assert response["branch_id"] == "main"
    assert response["run_id"] == run_id


def test_attack_graph_routes_are_typed_and_empty_without_contract_objects(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = service.start({"session_id": "attack-graph-http", "objective": "Inspect typed graph"}).single.run.run_id
        status, graph_body = _request(server, "GET", f"/api/runs/{run_id}/asset-attack-graph")
        assert status == 200
        status, paths_body = _request(server, "GET", f"/api/runs/{run_id}/attack-paths")

    graph = json.loads(graph_body)
    paths = json.loads(paths_body)
    assert graph["run_id"] == run_id
    assert graph["materialized"] is False
    assert graph["assets"] == []
    assert graph["findings"] == []
    assert graph["attack_paths"] == []
    assert paths["run_id"] == run_id
    assert paths["materialized"] is False
    assert paths["attack_paths"] == []


def test_fork_returns_active_branch_and_branch_heads(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = service.start({"session_id": "fork-meta-http", "objective": "Inspect fork metadata"}).single.run.run_id
        _, payload = _request(server, "GET", f"/api/conversations/{run_id}")
        entry_id = next(iter(json.loads(payload)["tree"]["nodes"]))
        status, payload = _request(
            server,
            "POST",
            f"/api/conversations/{run_id}/fork",
            json.dumps({"from_entry_id": entry_id, "branch_id": "review"}).encode(),
        )

    response = json.loads(payload)
    assert status == 200
    assert response["run_id"] == run_id
    assert response["active_branch_id"] == "review"
    assert response["branches"]["review"] == entry_id
    assert response["leaf_entry_id"] == entry_id


def test_browser_workbench_fits_narrow_mobile_viewport(tmp_path: Path) -> None:
    sync_api = pytest.importorskip("playwright.sync_api")
    with _server(tmp_path) as (server, _):
        try:
            browser = sync_api.sync_playwright().start()
            chromium = browser.chromium.launch(headless=True)
        except Exception as exc:
            pytest.skip(f"chromium unavailable: {exc}")
        page = chromium.new_page(viewport={"width": 320, "height": 640})
        try:
            page.goto(f"http://127.0.0.1:{server.server_port}/", wait_until="networkidle")
            assert page.evaluate("() => document.documentElement.scrollWidth <= document.documentElement.clientWidth")
        finally:
            page.close()
            chromium.close()
            browser.stop()


def test_environment_provider_configuration_never_requires_raw_cli_key() -> None:
    environment = {
        "TRACE_MODEL": "fixture-model",
        "TRACE_API_BASE_URL": "http://127.0.0.1:8000/v1",
        "TRACE_API_KEY_ENV": "FIXTURE_TOKEN",
        "FIXTURE_TOKEN": "top-secret",
        "TRACE_MODEL_CONTEXT_TOKENS": "64000",
        "TRACE_API_TIMEOUT_SECONDS": "15",
    }
    provider = model_provider_from_environment(environment)

    assert isinstance(provider, OpenAICompatibleProvider)
    assert provider.capabilities().max_context_tokens == 64_000
    assert provider.capabilities().metadata["model"] == "fixture-model"
    assert model_provider_from_environment({}) is None


def test_non_loopback_web_requires_tls_or_explicit_insecure_opt_in(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("TRACE_ADMIN_PASSWORD", "fixture-password")
    monkeypatch.delenv("TRACE_ALLOW_INSECURE_HTTP", raising=False)
    with pytest.raises(ValueError, match="requires_tls"):
        serve(tmp_path / "runtime", host="0.0.0.0", port=0)


def test_browser_workbench_controls_agent_service(tmp_path: Path) -> None:
    sync_api = pytest.importorskip("playwright.sync_api")
    with _server(tmp_path) as (server, service):
        try:
            browser = sync_api.sync_playwright().start()
            chromium = browser.chromium.launch(headless=True)
        except Exception as exc:
            pytest.skip(f"chromium unavailable: {exc}")
        console_errors: list[str] = []
        page = chromium.new_page(viewport={"width": 1440, "height": 900})
        page.on("console", lambda message: console_errors.append(message.text) if message.type == "error" else None)
        try:
            page.goto(f"http://127.0.0.1:{server.server_port}/", wait_until="networkidle")
            page.get_by_role("button", name="新建", exact=True).click()
            page.locator('[name="objective"]').fill("Prepare a plan")
            page.get_by_role("button", name="创建", exact=True).click()
            try:
                page.locator("#run-id").wait_for(state="visible", timeout=5000)
            except Exception as exc:
                notice = page.locator("#notice").inner_text()
                raise AssertionError(
                    f"create did not complete; notice={notice!r}; console={console_errors!r}"
                ) from exc
            run_id = page.locator("#run-id").inner_text()
            assert service.status(run_id).run.status == "running"

            page.locator('[data-command="run"]').click()
            page.locator("#run-status").filter(has_text="等待 Worker").wait_for()
            assert service.status(run_id).run.status == "waiting_worker"
            page.get_by_role("tab", name="攻击路径", exact=True).click()
            page.wait_for_function("() => document.querySelector('#attack-view')?.textContent.includes('run_id')", timeout=5000)
            assert '"run_id"' in page.locator("#attack-view").inner_text()

            page.get_by_role("button", name="预算", exact=True).click()
            page.locator('#budget-form [name="actions"]').fill("3")
            page.locator('#budget-form button[value="default"]').click()
            page.locator("#metric-actions").filter(has_text="/ 67").wait_for()
            assert service.status(run_id).run.budget.action_limit == 67

            page.locator('[data-command="pause"]').click()
            page.locator("#run-status").filter(has_text="已暂停").wait_for()
            assert service.status(run_id).run.status == "paused_budget"

            page.locator('[data-command="resume"]').click()
            page.locator("#run-status").filter(has_text="等待 Worker").wait_for()
            assert service.status(run_id).run.status == "waiting_worker"

            page.once("dialog", lambda dialog: dialog.accept())
            page.locator('[data-command="cancel"]').click()
            page.locator("#run-status").filter(has_text="已取消").wait_for()
            assert service.status(run_id).run.status == "cancelled"
            page.wait_for_function("() => state.eventCursor > 2", timeout=5000)

            assert page.locator("#empty-state").is_hidden()
            page.evaluate("window.scrollTo(0, 0)")
            page.screenshot(path=tmp_path / "trace-desktop.png")
            page.set_viewport_size({"width": 390, "height": 844})
            page.evaluate("window.scrollTo(0, 0)")
            page.screenshot(path=tmp_path / "trace-mobile.png")
            assert page.locator("body").evaluate("node => node.scrollWidth <= node.clientWidth")
            assert not console_errors
        finally:
            page.close()
            chromium.close()
            browser.stop()
