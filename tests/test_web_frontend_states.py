from __future__ import annotations

import json
from pathlib import Path

import pytest

from test_web_workbench import _request, _server
from test_web_workbench_races import _page


def test_adding_mcp_after_cancelled_edit_starts_with_empty_form(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, _):
        status, _ = _request(server, "POST", "/api/mcp", json.dumps({
            "server_id": "existing-server",
            "transport": "http",
            "url": "http://127.0.0.1:1",
        }).encode())
        assert status == 201
        with _page(server) as page:
            page.locator('[data-view="control"]').click()
            page.locator('[data-control-tab="mcp"]').click()
            page.locator('#mcp-list button').filter(has_text="编辑").click()
            assert page.locator('#mcp-form [name="server_id"]').input_value() == "existing-server"
            page.locator('#mcp-form button[value="cancel"]').last.click()
            page.locator('#add-mcp').click()
            assert page.locator('#mcp-form [name="server_id"]').input_value() == ""
            assert page.locator('#mcp-form [name="url"]').input_value() == ""
            assert page.locator('#mcp-form [name="transport"]').input_value() == "stdio"
            page.locator('#mcp-form [name="server_id"]').fill("new-server")
            page.locator('#mcp-form [name="env"]').fill("{")
            page.locator('#mcp-form button[value="default"]').click()
            assert page.locator('#mcp-dialog [role="alert"]').inner_text() == "env JSON 无效"
            assert page.locator('#mcp-dialog [role="alert"]').is_visible()


def test_login_remains_available_after_escape_and_explains_wrong_password(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("TRACE_ADMIN_PASSWORD", "fixture-password")
    with _server(tmp_path) as (server, _), _page(server) as page:
        assert page.locator('#login-dialog').is_visible()
        page.keyboard.press("Escape")
        assert page.locator('#login-dialog').is_visible()
        page.locator('#login-password').fill("wrong-password")
        page.locator('#login-submit').click()
        page.locator('#login-error').wait_for(state="visible")
        assert page.locator('#login-error').inner_text() == "密码不正确，请重试。"
        page.locator('#login-password').fill("fixture-password")
        page.locator('#login-submit').click()
        page.locator('#login-dialog').wait_for(state="hidden")
        assert page.locator('#runs-index').is_visible()


def test_registry_dossier_and_settings_fit_supported_viewports(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        service.start({"session_id": "layout-audit", "objective": "A" * 180})
        with _page(server) as page:
            assert page.locator('#logout').is_hidden()
            for width, height in [(1440, 900), (1024, 768), (390, 844), (320, 640)]:
                page.set_viewport_size({"width": width, "height": height})
                page.locator('[data-view="runs"]').click()
                assert page.locator('[data-view="runs"]').get_attribute("aria-pressed") == "true"
                assert page.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")
                page.locator('.run-item').click()
                page.locator('#workbench').wait_for(state="visible")
                assert page.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")
                assert page.locator('#metric-tokens').evaluate("node => node.scrollWidth <= node.clientWidth")
                page.locator('#back-to-runs').click()
                page.locator('[data-view="control"]').click()
                assert page.locator('[data-view="control"]').get_attribute("aria-pressed") == "true"
                for category in ["providers", "skills", "mcp", "conversations", "system"]:
                    page.locator(f'[data-control-tab="{category}"]').click()
                    assert page.locator(f'[data-control-tab="{category}"]').get_attribute("aria-pressed") == "true"
                    assert page.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")


def test_cairn_graph_and_pi_transcript_views_use_persisted_records(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = service.start({
            "session_id": "reference-ui",
            "objective": "验证真实搜索关系和连续会话",
            "targets": ["fixture://reference-ui"],
        }).single.run.run_id
        service.record_exploration(run_id, {
            "record_id": "search-origin",
            "hypothesis_id": "auth-boundary",
            "kind": "hypothesis",
            "status": "active",
            "statement": "认证边界可能接受过期会话",
            "target": "fixture://reference-ui",
        })
        service.record_exploration(run_id, {
            "record_id": "search-child",
            "hypothesis_id": "session-replay",
            "kind": "lead",
            "status": "proposed",
            "statement": "比较刷新前后的会话响应",
            "target": "fixture://reference-ui",
            "parent_record_ids": ["search-origin"],
            "capabilities": ["http_request"],
        })
        service.conversation.append(
            run_id=run_id,
            role="assistant",
            content="先建立基线，再比较刷新后的响应。",
            protected=False,
            source_type="model_response",
            source_id="response-1",
        )
        service.conversation.append(
            run_id=run_id,
            role="tool",
            content={
                "projection": {"structured_summary": {"status_code": 200}},
                "raw": {"artifact_ref": "artifact-fixture"},
            },
            protected=False,
            source_type="tool_result",
            source_id="tool-1",
        )

        status, payload = _request(server, "GET", f"/api/runs/{run_id}/search-graph")
        assert status == 200
        graph = json.loads(payload)
        assert [item["record_id"] for item in graph["records"]] == [
            "search-origin",
            "search-child",
        ]
        assert graph["records"][1]["parent_record_ids"] == ["search-origin"]

        with _page(server) as page:
            page.evaluate("id => loadRun(id)", run_id)
            page.locator('[data-tab="search"]').click()
            page.locator("#search-map .search-node").nth(1).wait_for()
            assert page.locator("#search-map .search-node").count() == 2
            page.locator("#search-map .search-node").nth(1).click()
            assert "search-origin" in page.locator("#search-inspector").inner_text()
            assert "http_request" in page.locator("#search-inspector").inner_text()
            assert page.locator("#search-raw").text_content().startswith("{")

            page.locator('[data-tab="transcript"]').click()
            page.locator("#transcript-list .transcript-tool").wait_for()
            assert page.locator("#transcript-list .transcript-assistant").count() == 1
            assert page.locator("#transcript-list .transcript-tool details").is_visible()
            assert not page.locator("#transcript-list .transcript-tool details").evaluate(
                "node => node.open"
            )

            page.set_viewport_size({"width": 320, "height": 640})
            page.locator('[data-tab="search"]').click()
            assert page.evaluate(
                "document.documentElement.scrollWidth <= document.documentElement.clientWidth"
            )
