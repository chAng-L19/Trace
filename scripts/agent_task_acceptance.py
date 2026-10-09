"""Deterministic model-loop acceptance against real local HTTP targets.

This measures the execution pipeline, not live-model reasoning or success rates.
"""
from __future__ import annotations

import json
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.asset_graph import project_asset_attack_graph
from redteam_agent.application.evidence_records import EvidenceRecords
from redteam_agent.core import Finding
from redteam_agent.providers import FakeModelProvider
from redteam_agent.runtime.operation_runtime import OperationRuntime


class Target(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        owner = self.headers.get("X-Fixture-Owner", "")
        allowed = owner == "1" and (self.path == "/resources/1" or self.server.vulnerable)
        status = 200 if allowed else 403
        body = json.dumps({"resource": self.path, "allowed": allowed}).encode()
        self.server.requests.append((self.path, owner, status))
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def accept_case(vulnerable, *, capture_write_delay=0):
    server = ThreadingHTTPServer(("127.0.0.1", 0), Target)
    server.vulnerable, server.requests = vulnerable, []
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    target = f"http://127.0.0.1:{server.server_port}"
    try:
        with tempfile.TemporaryDirectory(prefix="trace-task-acceptance-") as temporary:
            runtime = OperationRuntime(root=Path(temporary))
            service = None
            invocation_stages = []

            def state():
                return runtime.store.load_operation(run_id)

            def evidence(artifact_type):
                return next(node for node in runtime.evidence_graph.list(run_id)
                            if node.artifact_type == artifact_type)

            def fetch(path, owner="1"):
                descriptor = next(item for item in runtime.broker.descriptors()
                                  if item.qualified_name == "builtin:http-request")
                put_bytes = runtime.artifacts.put_bytes

                def write_capture(*args, **kwargs):
                    if (capture_write_delay and invocation_stages == ["map-surface"]
                            and kwargs.get("artifact_type", "").endswith("_raw")):
                        time.sleep(capture_write_delay)
                    return put_bytes(*args, **kwargs)

                # The socket deadline excludes capture persistence; the broker
                # retains its normal tool deadline, including the real writes.
                with patch.object(runtime.artifacts, "put_bytes", write_capture):
                    result = runtime.broker.call(descriptor, {
                        "url": target + path, "headers": {"X-Fixture-Owner": owner}, "timeout": 5,
                    }, run_id=run_id)
                assert result.status == "success", f"{result.tool}:{result.status}:{result.error}"
                return dict(result.output)

            def execute(arguments):
                stage = arguments["stage"]
                invocation_stages.append(stage)
                current = state()
                nodes = runtime.evidence_graph.list(run_id)
                refs = [node.evidence_id for node in nodes]
                action = next(action for action in runtime._workflow_for(current).actions
                              if action.action_id == stage)
                payload = {"target": target, "artifact_type": action.expected_artifact,
                           "evidence_refs": refs, "clause_ids": []}
                if stage == "map-surface":
                    own = fetch("/resources/1")
                    payload.update(assets=[target], routes=["/resources/1", "/resources/2"],
                                   baseline_status=own["status_code"])
                elif stage == "build-hypotheses":
                    payload["hypotheses"] = [{"id": "resource-owner", "priority": "high",
                        "statement": "Check whether owner 1 can read resource 2",
                        "evidence_refs": refs, "negative_control": "Anonymous request must be denied"}]
                elif stage == "validate-path":
                    exchanges = {"baseline": fetch("/resources/1"), "proof": fetch("/resources/2"),
                                 "control": fetch("/resources/2", owner="")}
                    measurements = {role + "_status": output["status_code"]
                                    for role, output in exchanges.items()}
                    assert measurements["baseline_status"] == 200 and measurements["control_status"] == 403
                    clauses = list(current.goal.intent_envelope["clause_ids"])
                    payload.update(reproducible=True, side_effects=False,
                        results=exchanges, measurements=measurements,
                        clause_ids=clauses,
                        clause_support={identity: {"measurements": measurements} for identity in clauses},
                        negative_controls=[{"source": "tool_output", "observation_path": "measurements.control_status",
                                            "expected": 403, "actual": measurements["control_status"], "passed": True}],
                        artifact_refs=[identity for output in exchanges.values()
                                       for identity in output.get("artifact_refs", ())])
                elif stage == "prove-impact":
                    measured = fetch("/resources/2")
                    reproduced = evidence("reproduction_artifact")
                    assert measured["status_code"] == reproduced.payload["measurements"]["proof_status"]
                    payload.update(measured=True, outcomes={"unauthorized_read": measured["status_code"] == 200},
                                   observations=measured, clause_ids=reproduced.payload["clause_ids"])
                elif stage == "review-coverage":
                    reproduced = evidence("reproduction_artifact")
                    payload.update(checked=["owner baseline", "cross-owner read", "anonymous denial"],
                        negative_controls=[{"observation_path": "measurements.control_status", "expected": 403,
                                            "actual": 403, "passed": True,
                                            "evidence_refs": [reproduced.evidence_id]}],
                        clause_ids=reproduced.payload["clause_ids"])
                elif stage == "cleanup":
                    own = fetch("/resources/1")
                    payload.update(verified=own["status_code"] == 200, outstanding_changes=[],
                                   checks={"owner_still_readable": own["status_code"] == 200,
                                           "requests_were_read_only": True},
                                   clause_ids=evidence("reproduction_artifact").payload["clause_ids"])
                elif stage == "report":
                    reproduced = evidence("reproduction_artifact")
                    findings = []
                    if evidence("impact_proof").payload["outcomes"]["unauthorized_read"]:
                        findings.append(Finding(finding_id="fixture-owner-bypass", run_id=run_id,
                            title="Cross-owner resource read", severity="high", status="verified", target=target,
                            reproduction_evidence_ids=(reproduced.evidence_id,),
                            impact_evidence_ids=(evidence("impact_proof").evidence_id,),
                            negative_control_evidence_ids=(reproduced.evidence_id,),
                            cleanup_evidence_ids=(evidence("cleanup_proof").evidence_id,)).to_dict())
                    payload.update(report="Local authorization measurements and controls", findings=findings,
                        goal_result="achieved", criteria=[{"criterion_id": criterion.criterion_id,
                            "status": "achieved", "evidence_refs": refs} for criterion in current.goal.success_criteria],
                        clause_results=[{"clause_id": identity, "target": target, "status": "achieved",
                                         "evidence_refs": refs} for identity in current.goal.intent_envelope["clause_ids"]])
                else:
                    raise AssertionError(stage)
                return payload

            runtime.broker.register_adapter(name="authorization", server="fixture", adapter=execute,
                capabilities=("target_intake", "reasoning", "controlled_validation", "impact_analysis",
                              "coverage_analysis", "cleanup", "report_generation"),
                input_schema={"type": "object", "properties": {"stage": {"type": "string"}},
                              "required": ["stage"], "additionalProperties": False})

            def model_turn(request):
                return {"status": "completed", "usage": {"total_tokens": 1},
                        "structured_output": {"commit_lifecycle_gate": True},
                        "tool_calls": [{"call_id": request.metadata["action_id"],
                                        "tool_name": "fixture:authorization",
                                        "arguments": {"stage": request.metadata["action_id"]}}]}

            provider = FakeModelProvider([model_turn] * 7)
            with AgentService(runtime=runtime, model_port=provider, load_external_configuration=False) as service:
                run_id = service.start({"session_id": "acceptance", "objective": "Verify local resource authorization",
                                        "targets": [target]}).single.run.run_id
                try:
                    result = service.run(run_id)
                except Exception as error:
                    failures = [item.to_dict() for item in service.journal.model_observations(run_id)
                                if item.status != "success"]
                    raise AssertionError({"stages": invocation_stages, "tool_failures": failures}) from error
                assert result.terminal.success and result.run.status == "completed", result.to_dict()
                assert len(provider.requests) == 7 and len(invocation_stages) == len(set(invocation_stages)) == 7
                graph = project_asset_attack_graph(service, run_id)
                assert len(graph["findings"]) == int(vulnerable), graph
                assert len(server.requests) == 6 and all(owner in {"", "1"} for _, owner, _ in server.requests)
                reproduced = evidence("reproduction_artifact")
                assert reproduced.payload["measurements"]["proof_status"] == (200 if vulnerable else 403)
                observation = next(item for item in service.journal.model_observations(run_id)
                                   if item.action_id == "validate-path")
                manifest_exchanges = []
                for role, exchange in reproduced.payload["results"].items():
                    parts = exchange["exchange_artifacts"]
                    manifest_exchanges.append({"role": role, "source_step_id": "model:" + observation.observation_id,
                        "request_artifact_id": parts["http_request"]["artifact_ref"],
                        "response_artifact_id": parts["http_response"]["artifact_ref"],
                        "response_body_artifact_id": parts["http_response_body"]["artifact_ref"]})
                    original = runtime.artifacts.read(parts["http_response_body"]["raw_capture"]["private_artifact_ref"], run_id=run_id)
                    assert json.loads(original)["allowed"] == (exchange["status_code"] == 200)
                records = EvidenceRecords(service)
                manifest = records.manifest(run_id, {"mode": "record", "manifest_id": "acceptance-http",
                                                     "target": target, "exchanges": manifest_exchanges})
                assert len(manifest["exchanges"]) == 3 and not manifest["runtime_verified"]
                sources = records.sources(run_id, target)
                report = runtime.artifacts.put_json(evidence("final_report").payload, run_id=run_id,
                                                     artifact_type="acceptance_report")
                revision = records.report(run_id, {"mode": "record", "report_id": "acceptance",
                    "revision_id": "v1", "target": target, "expected_source_hash": sources["source_hash"],
                    "content_artifact_id": report.artifact_id})
                assert not revision["stale"]
                return {"target": "vulnerable" if vulnerable else "clean", "completed": True,
                        "confirmed_findings": len(graph["findings"]), "http_requests": len(server.requests),
                        "duplicate_lifecycle_calls": len(invocation_stages) - len(set(invocation_stages)),
                        "model_tokens": sum(item.usage["total_tokens"] for item in service.journal.model_responses(run_id)),
                        "raw_exchange_bodies_verified": 3, "report_stale": revision["stale"]}
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def main():
    print(json.dumps({"ok": True, "model": "scripted_fixture_not_live_model",
                      "cases": [accept_case(True), accept_case(False, capture_write_delay=1.3)]}))


if __name__ == "__main__":
    main()
