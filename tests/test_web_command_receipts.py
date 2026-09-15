from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Event

import pytest

from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi, WebResponse
from redteam_agent.runtime.store_common import StoreConflictError


def test_concurrent_same_command_invokes_handler_once(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    entered, release = Event(), Event()
    calls: list[str] = []

    def execute(tail, body, *, command_id=""):
        calls.append(command_id)
        entered.set()
        assert release.wait(5)
        return WebResponse.json({"ok": True, "value": "saved"})

    monkeypatch.setattr(api, "_post_once", execute)
    try:
        with ThreadPoolExecutor(max_workers=8) as pool:
            first = pool.submit(api.dispatch, "POST", "/api/runs", body={"command_id": "concurrent"})
            assert entered.wait(5)
            try:
                retries = [pool.submit(api.dispatch, "POST", "/api/runs", body={"command_id": "concurrent"}) for _ in range(7)]
                assert all(future.result(timeout=5).status == 409 for future in retries)
            finally:
                release.set()
            response = first.result(timeout=5)
        replay = api.dispatch("POST", "/api/runs", body={"command_id": "concurrent"})
        assert response.body == replay.body
        assert calls == ["concurrent"]
    finally:
        release.set()
        service.close()


def test_receipt_takeover_fences_previous_generation_even_for_same_owner(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    try:
        store = service.runtime.store
        first = store.claim_web_command("fence", "hash", owner="same", ttl_seconds=1)
        with store.transaction(immediate=True) as connection:
            connection.execute("UPDATE web_command_receipts SET lease_expires_at=0 WHERE command_id='fence'")
        second = store.claim_web_command("fence", "hash", owner="same", ttl_seconds=1)
        assert second["fencing_token"] > first["fencing_token"]
        with pytest.raises(StoreConflictError):
            store.complete_web_command("fence", {"stale": True}, owner="same", fencing_token=first["fencing_token"])
        saved = store.complete_web_command("fence", {"current": True}, owner="same", fencing_token=second["fencing_token"])
        assert saved["response"] == {"current": True}
    finally:
        service.close()


def test_budget_side_effect_survives_receipt_failure_without_double_increment(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    root = tmp_path / "runtime"
    service = AgentService(root=root)
    run_id = service.start({"session_id": "budget-crash", "objective": "Prepare a plan"}).single.run.run_id
    before = service.status(run_id).run.budget.action_limit

    def fail_receipt(*args, **kwargs):
        raise RuntimeError("injected_receipt_crash")

    try:
        with monkeypatch.context() as patch:
            patch.setattr(service.runtime.store, "complete_web_command", fail_receipt)
            failed = WebApi(service).dispatch("POST", f"/api/runs/{run_id}/budget", body={"command_id": "budget-crash", "actions": 3})
        assert failed.status == 500
        assert service.status(run_id).run.budget.action_limit == before + 3
        with service.runtime.store.transaction(immediate=True) as connection:
            connection.execute("UPDATE web_command_receipts SET lease_expires_at=0 WHERE command_id='budget-crash'")
    finally:
        service.close()

    reopened = AgentService(root=root)
    try:
        retried = WebApi(reopened).dispatch("POST", f"/api/runs/{run_id}/budget", body={"command_id": "budget-crash", "actions": 3})
        assert retried.status == 200
        assert reopened.status(run_id).run.budget.action_limit == before + 3
    finally:
        reopened.close()


@pytest.mark.parametrize("command", ["run", "resume", "observation", "fork"])
def test_uncertain_command_never_reexecutes_after_receipt_failure(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    command: str,
) -> None:
    service = AgentService(root=tmp_path / "runtime")
    api = WebApi(service)
    calls: list[str] = []
    command_id = f"{command}-crash"
    path = f"/api/runs/run-1/{command}"

    def execute(tail, body, *, command_id=""):
        calls.append(command_id)
        return WebResponse.json({"ok": True})

    def fail_receipt(*args, **kwargs):
        raise RuntimeError("injected_receipt_crash")

    monkeypatch.setattr(api, "_post_once", execute)
    try:
        with monkeypatch.context() as patch:
            patch.setattr(service.runtime.store, "complete_web_command", fail_receipt)
            failed = api.dispatch("POST", path, body={"command_id": command_id})
        assert failed.status == 500
        with service.runtime.store.transaction(immediate=True) as connection:
            connection.execute(
                "UPDATE web_command_receipts SET lease_expires_at=0 WHERE command_id=?",
                (command_id,),
            )
        retried = api.dispatch("POST", path, body={"command_id": command_id})
        assert retried.status == 409
        assert retried.payload()["error"] == "command_result_uncertain"
        assert calls == [command_id]
    finally:
        service.close()
