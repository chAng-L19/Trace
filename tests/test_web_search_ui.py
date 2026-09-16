from __future__ import annotations

from pathlib import Path
from threading import Event

import pytest

from test_web_workbench import _server
from test_web_workbench_races import _page, _run


def test_search_history_and_selection_survive_refresh(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = _run(service, "search-history")
        for record_id, status in [("old", "active"), ("current", "suspended")]:
            service.record_exploration(run_id, {
                "record_id": record_id, "hypothesis_id": "same-direction",
                "kind": "hypothesis", "status": status,
                "statement": f"State {status}",
            })
        with _page(server) as page:
            page.evaluate("id => loadRun(id)", run_id)
            page.locator('[data-tab="search"]').click()
            page.locator('[data-record-id="current"]').wait_for()
            assert page.locator('[data-lane="active"] .search-node').count() == 0
            assert page.locator('[data-lane="history"] [data-record-id="old"]').count() == 1
            assert page.locator('[data-lane="suspended"] [data-record-id="current"]').count() == 1
            node = page.locator('[data-record-id="old"]')
            node.focus()
            page.keyboard.press("Enter")
            page.locator('#search-inspector summary').click()
            page.evaluate("() => loadTab('search')")
            assert node.get_attribute("aria-pressed") == "true"
            assert page.locator('#search-inspector details').evaluate("node => node.open")
            assert "old" in page.locator('#search-inspector').inner_text()


def test_graph_and_transcript_boundaries_remain_readable_and_inert(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = _run(service, "long-search-content")
        unsafe_text = '<img src=x onerror="window.auditExecuted=true">' + "UnbrokenIdentifier" * 30
        service.record_exploration(run_id, {
            "record_id": "long-node", "hypothesis_id": "long-hypothesis",
            "kind": "hypothesis", "status": "active", "statement": unsafe_text,
        })
        service.conversation.append(
            run_id=run_id, role="tool", content={"result": unsafe_text, "lines": list(range(20))},
            protected=False, source_type="longSourceType" * 30, source_id="tool-response",
        )
        with _page(server) as page:
            page.evaluate("id => loadRun(id)", run_id)
            for width, height in [(1440, 900), (1024, 768), (390, 844), (320, 640)]:
                page.set_viewport_size({"width": width, "height": height})
                page.locator('[data-tab="search"]').click()
                page.locator('[data-record-id="long-node"]').wait_for()
                assert page.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")
                page.locator('[data-record-id="long-node"]').click()
                assert unsafe_text in page.locator('#search-inspector').inner_text()
                assert page.locator('#search-map img, #search-inspector img').count() == 0
                if width < 960:
                    assert page.locator('#search-inspector h3').evaluate(
                        "node => node.getBoundingClientRect().top >= 0 && node.getBoundingClientRect().top < innerHeight"
                    )
                    page.locator('.inspector-back').click()
                    assert page.locator('[data-record-id="long-node"]').evaluate("node => node === document.activeElement")
                page.locator('[data-tab="transcript"]').click()
                page.locator('.transcript-tool').wait_for()
                assert page.evaluate("document.documentElement.scrollWidth <= document.documentElement.clientWidth")
                assert page.locator('#transcript-list img').count() == 0
                assert page.evaluate("typeof window.auditExecuted === 'undefined'")
            details = page.locator('.transcript-tool details')
            details.locator('summary').click()
            page.evaluate("() => loadTab('transcript')")
            assert details.evaluate("node => node.open")


def test_search_legacy_projection_deduplicates_contradictions_and_marks_window(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service), _page(server) as page:
        run_id = _run(service, "projection-compatibility")
        page.evaluate("id => loadRun(id)", run_id)
        record = {"record_id": "conflict", "kind": "contradiction", "status": "active", "statement": "Compare results"}
        page.evaluate("""payload => TraceUI.renderSearchGraph(
            document.querySelector('#search-map'), document.querySelector('#search-inspector'),
            document.querySelector('#search-raw'), payload)""", {
                "search_graph": {"active": [record], "unresolved_contradictions": [record], "record_count": 1},
            })
        assert page.locator('#search-map .search-node').count() == 1
        assert page.locator('[data-lane="contradictions"] .search-node').count() == 1
        page.evaluate("""payload => TraceUI.renderSearchGraph(
            document.querySelector('#search-map'), document.querySelector('#search-inspector'),
            document.querySelector('#search-raw'), payload)""", {
                "search_graph": {"record_count": 257}, "records": [record], "records_truncated": True,
            })
        assert page.locator('#search-count').text_content() == "最近 1 / 共 257 条记录"


def test_switching_runs_discards_delayed_search_graph(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    with _server(tmp_path) as (server, service):
        old_id, new_id = _run(service, "old-search"), _run(service, "new-search")
        for run_id, record_id in [(old_id, "old-record"), (new_id, "new-record")]:
            service.record_exploration(run_id, {"record_id": record_id, "statement": record_id})
        original = server.api._get
        entered, release = Event(), Event()

        def delayed_get(tail, query):
            if tail == [old_id, "search-graph"]:
                entered.set()
                assert release.wait(5)
            return original(tail, query)

        monkeypatch.setattr(server.api, "_get", delayed_get)
        with _page(server) as page:
            try:
                page.evaluate("id => loadRun(id)", old_id)
                page.evaluate("() => { window.oldSearch = loadTab('search'); }")
                assert entered.wait(5)
                page.evaluate("id => loadRun(id)", new_id)
                release.set()
                page.evaluate("() => window.oldSearch")
                assert page.locator('[data-record-id="new-record"]').count() == 1
                assert page.locator('[data-record-id="old-record"]').count() == 0
            finally:
                release.set()
