from __future__ import annotations

from contextlib import contextmanager
import json
from pathlib import Path
from threading import Event

import pytest

from test_web_workbench import _server
from test_agent_service import _plan_request


@contextmanager
def _page(server):
    sync_api = pytest.importorskip("playwright.sync_api")
    with sync_api.sync_playwright() as playwright:
        browser = playwright.chromium.launch(headless=True)
        page = browser.new_page()
        try:
            page.goto(f"http://127.0.0.1:{server.server_port}/", wait_until="networkidle")
            yield page
        finally:
            page.close()
            browser.close()


def _run(service, name: str) -> str:
    return service.start({"session_id": name, "objective": f"Prepare a plan for {name}"}).single.run.run_id


def test_switching_runs_discards_delayed_initial_events(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    with _server(tmp_path) as (server, service):
        newer_selection = _run(service, "earlier-sequence")
        stale_selection = _run(service, "later-sequence")
        original = service.events
        entered, release = Event(), Event()

        def delayed_events(run_id, *args, **kwargs):
            if run_id == stale_selection:
                entered.set()
                assert release.wait(5)
            return original(run_id, *args, **kwargs)

        monkeypatch.setattr(service, "events", delayed_events)
        with _page(server) as page:
            try:
                page.evaluate("id => { window.oldLoad = loadRun(id); }", stale_selection)
                assert entered.wait(5)
                page.evaluate("id => loadRun(id)", newer_selection)
                release.set()
                page.evaluate("() => window.oldLoad")
                result = page.evaluate("() => ({selected: state.selectedId, events: state.events, cursor: state.eventCursor})")
                assert result["selected"] == newer_selection
                assert all(event["run_id"] == newer_selection for event in result["events"])
                assert result["cursor"] == original(newer_selection)[-1].sequence
            finally:
                release.set()


def test_switching_runs_discards_delayed_report(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    with _server(tmp_path) as (server, service):
        old_id, new_id = _run(service, "old-report"), _run(service, "new-report")
        original = server.api._get
        entered, release = Event(), Event()

        def delayed_get(tail, query):
            if tail == [old_id, "report"]:
                entered.set()
                assert release.wait(5)
            return original(tail, query)

        monkeypatch.setattr(server.api, "_get", delayed_get)
        with _page(server) as page:
            try:
                page.evaluate("id => loadRun(id)", old_id)
                page.evaluate("() => { window.oldReport = loadTab('report'); }")
                assert entered.wait(5)
                page.evaluate("id => loadRun(id)", new_id)
                release.set()
                page.evaluate("() => window.oldReport")
                report = page.evaluate("() => state.report")
                assert report["run"]["run"]["run_id"] == new_id
                assert old_id not in page.locator("#report-view").inner_text()
            finally:
                release.set()


def test_cancel_create_dialog_ignores_invalid_required_fields(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, _), _page(server) as page:
        page.locator("#new-run").click()
        page.locator('#create-form button[value="cancel"]').last.click()
        assert not page.locator("#create-dialog").is_visible()


def test_unknown_usage_is_not_displayed_as_zero(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = _run(service, "unknown-usage")
        state = service.runtime.store.load_operation(run_id)
        state.budget.token_limit = 100
        state.budget.input_tokens_used = None
        state.budget.output_tokens_used = None
        state.budget.token_usage_missing = 1
        state.budget.pause("token_usage_unknown")
        state.status = "paused_budget"
        service.runtime.store.save_operation(state, expected_version=state.state_version)
        with _page(server) as page:
            page.evaluate("id => loadRun(id)", run_id)
            assert "未知" in page.locator("#metric-tokens").inner_text()
            assert page.locator("#tokens-progress").get_attribute("value") is None


def test_refresh_keeps_in_flight_command_disabled(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    with _server(tmp_path) as (server, service):
        run_id = _run(service, "slow-run")
        original = service.run
        entered, release = Event(), Event()

        def delayed_run(*args, **kwargs):
            entered.set()
            assert release.wait(5)
            return original(*args, **kwargs)

        monkeypatch.setattr(service, "run", delayed_run)
        with _page(server) as page:
            try:
                page.evaluate("id => loadRun(id)", run_id)
                page.locator('[data-command="run"]').click()
                assert entered.wait(5)
                page.evaluate("id => loadRun(id, false)", run_id)
                assert page.locator('[data-command="run"]').is_disabled()
            finally:
                release.set()


def test_newer_status_version_survives_late_response(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = _run(service, "versions")
        with _page(server) as page:
            page.evaluate("id => loadRun(id)", run_id)
            result = page.evaluate("""() => {
                const newer = structuredClone(state.view);
                newer.run.state_version = 20;
                newer.run.status = 'cancelled';
                applyView(newer);
                const older = structuredClone(newer);
                older.run.state_version = 19;
                older.run.status = 'running';
                const accepted = applyView(older);
                renderView();
                return {accepted, status: state.view.run.status};
            }""")
            assert result == {"accepted": False, "status": "cancelled"}
            assert page.locator('[data-command="run"]').is_disabled()


def test_transcript_can_load_beyond_first_page_and_tabs_support_keyboard(tmp_path: Path) -> None:
    with _server(tmp_path) as (server, service):
        run_id = _run(service, "long-transcript")
        for index in range(205):
            service.conversation.append(run_id=run_id, role="assistant", content=f"message-{index}", protected=False, source_type="fixture", source_id=str(index))
        with _page(server) as page:
            page.evaluate("id => loadRun(id)", run_id)
            page.locator('[data-tab="events"]').focus()
            page.keyboard.press("ArrowRight")
            assert page.locator('[data-tab="search"]').get_attribute("aria-selected") == "true"
            assert page.locator('#panel-search').get_attribute("aria-labelledby") == "tab-search"
            page.locator('[data-tab="transcript"]').click()
            page.locator('[data-more="transcript"]').wait_for(state="visible")
            assert page.locator("#transcript-list .record").count() == 200
            page.locator('[data-more="transcript"]').click()
            page.locator('[data-more="transcript"]').wait_for(state="hidden")
            assert page.locator("#transcript-list .record").count() == len(service.transcript(run_id))
            assert "message-204" in page.locator("#transcript-list").inner_text()


def test_completed_run_panels_and_downloads_use_persisted_records(tmp_path: Path) -> None:
    target = tmp_path / "target.txt"
    target.write_text("workbench fixture", encoding="utf-8")
    with _server(tmp_path) as (server, service):
        run_id = service.start(_plan_request("panels", target)).single.run.run_id
        completed = service.run(run_id)
        assert completed.terminal.success
        artifact = service.runtime.artifacts.put_bytes(b"verified download", run_id=run_id)
        with _page(server) as page:
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.on("console", lambda message: errors.append(message.text) if message.type == "error" else None)
            page.on("response", lambda response: errors.append(f"HTTP {response.status}: {response.url}") if response.status >= 400 else None)
            page.evaluate("id => loadRun(id)", run_id)
            page.locator('[data-tab="search"]').click()
            page.wait_for_function("() => document.querySelector('#search-view').textContent.length > 0")
            page.locator('[data-tab="evidence"]').click()
            page.wait_for_function("() => document.querySelectorAll('#evidence-list .record').length > 0")
            assert page.locator("#evidence-list .record").count() == len(completed.evidence)
            page.locator('[data-tab="artifacts"]').click()
            download_link = page.locator(f'#artifact-list a[href*="{artifact.artifact_id}"]')
            with page.expect_download() as download_info:
                download_link.click()
            downloaded = download_info.value
            assert Path(downloaded.path()).read_bytes() == b"verified download"
            page.locator('[data-tab="transcript"]').click()
            page.wait_for_function("() => document.querySelectorAll('#transcript-list .record').length > 0")
            assert page.locator("#transcript-list .record").count() == len(service.transcript(run_id))
            page.locator('[data-tab="report"]').click()
            page.wait_for_function("() => state.report !== null")
            with page.expect_download() as report_info:
                page.locator("#export-report").click()
            report = json.loads(Path(report_info.value.path()).read_text(encoding="utf-8"))
            assert report["run"]["run"]["run_id"] == run_id
            assert report["terminal"]["success"] is True
            assert not errors
