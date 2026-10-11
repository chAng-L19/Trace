"""Offline terminal discussion, persistence, branch and shutdown regressions."""
from __future__ import annotations

import json
import sys
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch
from uuid import uuid4

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent import AgentService, StartRequest
from redteam_agent.core import Finding, ModelCapabilities, ModelResponse
from redteam_agent.application.model_loop import AgentLoop
from redteam_agent.runtime.store_common import ImmutableRecordError


class Stub:
    def __init__(self):
        self.requests = []
        self.cancelled = []
        self.entered = threading.Event()
        self.release = threading.Event()
        self.block = False
        self.ignore_cancel = False
        self.mode = "ok"

    def capabilities(self):
        return ModelCapabilities(max_context_tokens=128000, metadata={"model": "local-stub", "provider": "stub"})

    def complete(self, request):
        assert not request.tools and not request.allow_parallel_tools and not request.response_schema
        self.requests.append(request)
        self.entered.set()
        if self.block:
            assert self.release.wait(10), "stub release timed out"
        if self.mode == "error":
            raise RuntimeError("stub_transport_failure")
        response = ModelResponse(request_id=request.request_id, status="completed", text="Local answer",
                                 provider="stub", model="local-stub", usage={"total_tokens": 12})
        if self.mode == "summary":
            summary = json.loads(request.messages[1]["content"])["terminal_result"]
            titles = ", ".join(item["title"] for item in summary["verified_findings"]) or "No verified findings"
            return replace(response, text=f"{summary['goal']['objective']} | {summary['status']} | "
                           f"{summary['terminal']['reason']} | {titles}")
        if self.mode == "tools":
            return replace(response, tool_calls=({"id": "tool-1", "name": "must_not_execute", "arguments": {}},))
        if self.mode == "mismatch":
            return replace(response, request_id="wrong-request")
        if self.mode == "bad-hash":
            return replace(response, response_hash="incorrect")
        if self.mode == "bad-usage":
            return replace(response, usage={"total_tokens": -1})
        if self.mode == "failed":
            return replace(response, status="failed", error="stub_failed_response")
        return response

    def cancel(self, request_id):
        self.cancelled.append(request_id)
        if not self.ignore_cancel:
            self.release.set()
        return True


def terminal(service, status="completed"):
    run_id = service.start(StartRequest(session_id="discussion-" + uuid4().hex, objective="Explain the fixture result",
        targets=("fixture://discussion",))).single.run.run_id
    state = service.runtime.store.load_operation(run_id)
    state.status = status
    service.runtime.store.save_operation(state)
    return run_id


def operation(service, run_id):
    with service.runtime.store.connection() as connection:
        return tuple(connection.execute("SELECT * FROM operations WHERE run_id=?", (run_id,)).fetchone())


def counts(service, run_id):
    store = service.runtime.store
    return tuple(len(f(run_id)) for f in (store.conversation_messages, store.model_requests, store.model_responses, service.journal.entries))


def rejects(callback, error, contains):
    try:
        callback()
    except error as exc:
        assert contains in str(exc), str(exc)
    else:
        raise AssertionError("expected " + contains)


def main():
    checks = []
    with tempfile.TemporaryDirectory(prefix="trace-discussion-") as temporary:
        root = Path(temporary)
        model = Stub()
        service = AgentService(root=root, model_port=model, load_external_configuration=False)
        try:
            with patch.object(service, "_resume_runtime", side_effect=AssertionError("runtime rerun")), \
                 patch.object(AgentLoop, "run", side_effect=AssertionError("agent loop executed")), \
                 patch.object(service.tools, "invoke", side_effect=AssertionError("tool executed")), \
                 patch.object(service, "_record_model_usage", side_effect=AssertionError("budget modified")):
                for status in ("completed", "failed", "cancelled"):
                    run_id = terminal(service, status)
                    before, total = operation(service, run_id), counts(service, run_id)
                    result = service.discuss(run_id, "Why this result?", message_id="once")
                    assert result["message"]["role"] == "user" and result["response"]["text"] == "Local answer"
                    assert result["response"]["status"] == "completed"
                    stored_response = service.runtime.store.model_responses(run_id)[-1]
                    stored_request = service.runtime.store.model_requests(run_id)[-1]
                    assert stored_response.usage == {"total_tokens": 12}
                    assert stored_request.request["metadata"]["discussion"] is True
                    assert counts(service, run_id) == tuple(a + b for a, b in zip(total, (2, 1, 1, 4))), (total, counts(service, run_id))
                    assert operation(service, run_id) == before
                    after, requests = counts(service, run_id), len(model.requests)
                    assert service.discuss(run_id, "Why this result?", message_id="once") == result
                    assert counts(service, run_id) == after and len(model.requests) == requests
                    rejects(lambda: service.discuss(run_id, "Changed text", message_id="once"),
                            ImmutableRecordError, "immutable_conversation_message")
                    assert counts(service, run_id) == after
                checks += ["all-terminal-states-unchanged", "exact-idempotent-replay", "changed-text-conflict", "no-runtime-tools-budget", "usage-recorded-without-budget-accounting"]

                # Seed actual gated evidence, not a model assertion of a finding.
                from reporting_smoke import persist
                report_run = terminal(service)
                state = service.runtime.store.load_operation(report_run)
                state.terminal_reason = "fixture conclusion recorded"
                service.runtime.store.save_operation(state)
                runtime = service.runtime
                source = persist(runtime, state, "summary-surface", "surface_map", {"assets": [state.goal.targets[0]]})
                repro = persist(runtime, state, "summary-repro", "reproduction_artifact", {
                    "reproducible": True, "tests": ["Read a local fixture"], "side_effects": False,
                    "body": "RAW_EVIDENCE_MUST_NOT_ENTER_PROMPT" * 1000,
                    "control": {"matches": 0}, "negative_controls": [{"source": "tool_output",
                        "observation_path": "control.matches", "expected": 0, "actual": 0, "passed": True}],
                }, parents=(source.evidence_id,))
                impact = persist(runtime, state, "summary-impact", "impact_proof",
                    {"verified": True, "impact": {"readable_bytes": 7}}, parents=(repro.evidence_id,))
                cleanup = persist(runtime, state, "summary-cleanup", "cleanup_proof",
                    {"cleaned": True, "side_effects": False}, parents=(repro.evidence_id,))
                finding = Finding("summary-finding", report_run, "Verified fixture conclusion", "medium",
                    status="verified", target=state.goal.targets[0], description="The local observation was verified.",
                    reproduction_evidence_ids=(repro.evidence_id,), impact_evidence_ids=(impact.evidence_id,),
                    negative_control_evidence_ids=(repro.evidence_id,),
                    cleanup_evidence_ids=(cleanup.evidence_id,),
                    metadata={"arbitrary_blob": "METADATA_MUST_NOT_ENTER_PROMPT" * 1000})
                unverified = replace(finding, finding_id="unverified", title="Ungated assertion", reproduction_evidence_ids=())
                persist(runtime, state, "summary-finding", "surface_map", {"findings": [finding.to_dict(), unverified.to_dict()]})
                before = operation(service, report_run)
                evidence_before = [item.to_dict() for item in runtime.evidence_graph.list(report_run)]
                model.mode = "summary"
                result = service.discuss(report_run, "What was concluded?", message_id="summary-1")
                summary = json.loads(model.requests[-1].messages[1]["content"])["terminal_result"]
                assert [item["finding_id"] for item in summary["verified_findings"]] == ["summary-finding"]
                assert summary["terminal"] == {key: service.status(report_run).terminal.to_dict()[key] for key in summary["terminal"]}
                assert summary["goal"]["targets"] == list(state.goal.targets)
                assert summary["verified_findings"][0]["reproduction_evidence_ids"] == [repro.evidence_id]
                assert "Verified fixture conclusion" in result["response"]["text"]
                assert summary["terminal"]["reason"] in result["response"]["text"]
                for marker in ("RAW_EVIDENCE_MUST_NOT_ENTER_PROMPT", "METADATA_MUST_NOT_ENTER_PROMPT", "Ungated assertion"):
                    assert marker not in json.dumps(model.requests[-1].to_dict())
                assert len(json.dumps(summary)) < 4000
                service.discuss(report_run, "Explain again", message_id="summary-2")
                assert sum("terminal_result" in str(item["content"]) for item in model.requests[-1].messages) == 1
                assert not any(item.source_type == "model_request_projection" for item in service.conversation.messages(report_run))
                assert operation(service, report_run) == before
                assert [item.to_dict() for item in runtime.evidence_graph.list(report_run)] == evidence_before
                for status in ("failed", "cancelled"):
                    empty_run = terminal(service, status)
                    result = service.discuss(empty_run, "What was concluded?", message_id="empty-summary")
                    summary = json.loads(model.requests[-1].messages[1]["content"])["terminal_result"]
                    assert summary["status"] == status and not summary["terminal"]["success"]
                    assert not summary["verified_findings"] and "No verified findings" in result["response"]["text"]
                model.mode = "ok"
                checks += ["terminal-goal-verified-conclusions-visible-in-reply", "summary-excludes-raw-evidence-and-ungated-claims",
                           "summary-read-only-and-not-recursively-projected", "failed-cancelled-empty-findings-summary"]

                run_id = terminal(service)
                branch_point = service.journal.leaf_id(run_id)
                original = service.journal.active_branch_id(run_id)
                total = counts(service, run_id)
                service.conversation.append(run_id=run_id, role="system", content="projection-must-not-recur",
                    protected=False, source_type="model_request_projection", source_id="legacy-projection")
                first = service.discuss(run_id, "main-only-question", message_id="shared-id")
                for index in range(3):
                    service.discuss(run_id, f"followup-{index}", message_id=f"followup-{index}")
                lengths = [len(item.messages) for item in model.requests[-4:]]
                assert lengths == list(range(lengths[0], lengths[0] + 8, 2)), lengths
                assert "projection-must-not-recur" not in json.dumps(model.requests[-1].to_dict())
                assert counts(service, run_id)[0] == total[0] + 9
                service.fork_session(run_id, branch_point, "alternate")
                alternate = service.discuss(run_id, "alternate-question", message_id="shared-id")
                assert alternate["response"]["request_id"] != first["response"]["request_id"]
                assert "main-only-question" not in json.dumps(model.requests[-1].to_dict())
                assert all(entry.branch_id == "alternate" for entry in service.journal.replay(run_id)[-4:])
                service.checkout_session(run_id, original)
                assert service.discuss(run_id, "main-only-question", message_id="shared-id") == first
                checks += ["branch-scoped-history-and-identity", "linear-projection-growth"]

                before = operation(service, run_id)
                for mode in ("error", "tools", "mismatch", "bad-hash", "bad-usage", "failed"):
                    model.mode = mode
                    result = service.discuss(run_id, "failure " + mode, message_id=mode)
                    assert result["response"]["status"] == "failed", result
                    assert result["response"]["error"]
                    size = len(model.requests)
                    assert service.discuss(run_id, "failure " + mode, message_id=mode) == result
                    assert len(model.requests) == size and operation(service, run_id) == before
                    assert service.conversation.messages(run_id)[-1].role == "assistant"
                model.mode = "ok"
                checks.append("durable-failures-and-replay")

                running = terminal(service, "running")
                size = counts(service, running)
                rejects(lambda: service.discuss(running, "no", message_id="no"), ValueError, "discussion_requires_terminal")
                assert counts(service, running) == size
                rejects(lambda: service.discuss(run_id, "", message_id="empty"), ValueError, "message_text_required")
                rejects(lambda: service.discuss(run_id, "text", message_id=""), ValueError, "message_id_required")
                service.configure_model(None)
                assert service.discuss(run_id, "main-only-question", message_id="shared-id") == first
                rejects(lambda: service.discuss(run_id, "new", message_id="no-model"), ValueError, "model_provider_required")
                service.configure_model(model)
                checks.append("validation-and-replay-without-provider")

                # A checkout while inference is blocked must not move its answer.
                model.block, model.entered, model.release = True, threading.Event(), threading.Event()
                with ThreadPoolExecutor(max_workers=2) as pool:
                    future = pool.submit(service.discuss, run_id, "branch-inflight", message_id="inflight")
                    assert model.entered.wait(5)
                    service.checkout_session(run_id, "alternate")
                    model.release.set()
                    inflight = future.result(5)
                assert service.journal.active_branch_id(run_id) == "alternate"
                assert all(item.source_id != inflight["response"]["request_id"] for item in service.conversation.messages(run_id))
                service.checkout_session(run_id, original)
                assert service.conversation.messages(run_id)[-1].source_id == inflight["response"]["request_id"]
                checks.append("checkout-during-inference-pins-origin")

                model.entered, model.release = threading.Event(), threading.Event()
                size = len(model.requests)
                with ThreadPoolExecutor(max_workers=2) as pool:
                    one = pool.submit(service.discuss, run_id, "concurrent", message_id="concurrent")
                    assert model.entered.wait(5)
                    two = pool.submit(service.discuss, run_id, "concurrent", message_id="concurrent")
                    model.release.set()
                    assert one.result(5) == two.result(5)
                assert len(model.requests) == size + 1
                checks.append("concurrent-replay-one-model-call")

                model.entered, model.release = threading.Event(), threading.Event()
                before = operation(service, run_id)
                with ThreadPoolExecutor(max_workers=1) as pool:
                    future = pool.submit(service.discuss, run_id, "cancel follow-up", message_id="cancel-followup")
                    assert model.entered.wait(5)
                    service._interrupt_model_loops(run_id, cancel=True)
                    cancelled = future.result(5)
                assert cancelled["response"]["status"] == "interrupted"
                assert operation(service, run_id) == before
                assert service.discuss(run_id, "cancel follow-up", message_id="cancel-followup") == cancelled
                checks.append("model-cancellation-hook-and-replay")

            service.close()
            service = AgentService(root=root, model_port=model, load_external_configuration=False)
            assert service.discuss(run_id, "main-only-question", message_id="shared-id") == first
            checks.append("restart-replay")

            # A crash after request persistence cannot safely retry the paid call.
            with patch.object(model, "complete", side_effect=KeyboardInterrupt):
                rejects(lambda: service.discuss(run_id, "orphan", message_id="orphan"), KeyboardInterrupt, "")
            size = len(model.requests)
            orphan = service.discuss(run_id, "orphan", message_id="orphan")
            assert orphan["response"]["error"] == "discussion_outcome_unknown"
            assert len(model.requests) == size
            checks.append("orphan-request-not-resent")

            model.entered, model.release = threading.Event(), threading.Event()
            before = operation(service, run_id)
            with ThreadPoolExecutor(max_workers=1) as pool:
                future = pool.submit(service.discuss, run_id, "shutdown", message_id="shutdown")
                assert model.entered.wait(5)
                service.close(timeout_seconds=5)
                result = future.result(5)
            assert result["response"]["status"] == "interrupted" and model.cancelled
            assert operation(service, run_id) == before
            rejects(lambda: service.discuss(run_id, "closed", message_id="closed"), RuntimeError, "agent_service_closing")
            checks.append("shutdown-cancels-without-changing-terminal")

            # An uncooperative provider leaves close timed out, not falsely done.
            model = Stub()
            model.block = model.ignore_cancel = True
            service = AgentService(root=root, model_port=model, load_external_configuration=False)
            with ThreadPoolExecutor(max_workers=1) as pool:
                future = pool.submit(service.discuss, run_id, "late", message_id="late")
                assert model.entered.wait(5)
                rejects(lambda: service.close(timeout_seconds=0.1), TimeoutError, "agent_service_shutdown_timeout")
                assert not service._close_complete.is_set()
                model.release.set()
                assert future.result(5)["response"]["status"] == "interrupted"
                assert service._close_complete.wait(5)
            assert operation(service, run_id) == before
            checks.append("uncooperative-shutdown-timeout")
        finally:
            model.release.set()
            service.close()
    print(json.dumps({"ok": True, "checks": checks}))


if __name__ == "__main__":
    main()
