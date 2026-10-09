"""Offline credential-channel regression: real journal, fake model/tool only."""
from __future__ import annotations

import json
import sys
import tempfile
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.adapters.runtime import RuntimeToolAdapter
from redteam_agent.application.context import ConversationLedger
from redteam_agent.application.model_loop import AgentLoop, ModelIntegrityError
from redteam_agent.application.model_stream import invoke_model_stream
from redteam_agent.core import ModelRequest, ModelResponse, ModelStreamEvent, ToolCall, contract_hash
from redteam_agent.runtime.operation_runtime import OperationRuntime
from redteam_agent.runtime.security import CredentialVault, secret_reference
from redteam_agent.runtime.session_journal import ModelRequestRecord


SECRET = "fixture-private-secret-credential-channel-123"
BUNDLE = json.dumps({"access_key_id": "fixture-access-id-123", "secret_access_key": SECRET})


class FakeProvider:
    def complete(self, request):
        response = ModelResponse(
            request_id=request.request_id, status="completed", provider="offline",
            tool_calls=({"id": "call", "name": "offline:cloud-inventory",
                         "arguments": json.dumps({"credential_ref": BUNDLE})},),
            usage={}, finish_reason="tool_calls",
        )
        return replace(response, response_hash=contract_hash(AgentLoop._response_projection(response)))

    def stream(self, request):
        # Split inside the secret so per-delta regular expressions cannot help.
        text = 'secret_access_key="' + SECRET + '"'
        pieces = (text[:30], text[30:])
        for index, piece in enumerate(pieces):
            yield ModelStreamEvent(request_id=request.request_id, sequence=index,
                                   event_type="text_delta", payload={"delta": piece})
        yield ModelStreamEvent(request_id=request.request_id, sequence=2,
                               event_type="tool_call", payload=self.complete(request).tool_calls[0])
        yield ModelStreamEvent(request_id=request.request_id, sequence=3,
                               event_type="completed", payload={"text": text, "finish_reason": "tool_calls"})


def request_for(runtime, run_id, identity):
    request = ModelRequest(request_id=identity, run_id=run_id, messages=())
    runtime.store.save_model_request(ModelRequestRecord(
        request_id=identity, run_id=run_id, prompt_hash=contract_hash(AgentLoop._prompt_projection(request)),
        provider="offline", model="", capabilities={}, request=request.to_dict(),
        created_at="2026-10-08T00:00:00+00:00",
    ))
    return request


def expect_rejected(action, message):
    try:
        action()
    except ValueError as error:
        assert str(error) == message, str(error)
    else:
        raise AssertionError("expected rejection: " + message)


def main():
    with tempfile.TemporaryDirectory(prefix="trace-credential-channel-") as directory:
        root = Path(directory)
        runtime = OperationRuntime(root=root, register_builtins=False)
        first = runtime.start(session_id="first", objective="Inspect local cloud credentials",
                              targets=("fixture.invalid",), model_led=True)
        second = runtime.start(session_id="second", objective="Inspect local cloud credentials",
                               targets=("fixture.invalid",), model_led=True)
        observed = []

        def echo(arguments):
            observed.append(arguments["credential_ref"])
            assert arguments["credential_ref"] == BUNDLE
            return {"echo": BUNDLE, "principal": SECRET, "secret_access_key": SECRET}

        runtime.broker.register_adapter(
            name="cloud-inventory", server="offline", adapter=echo, reconciler=lambda args: echo(args["arguments"]),
            capabilities=("cloud_inventory",), description="Offline credential probe",
            input_schema={"type": "object", "properties": {"credential_ref": {"type": "string"}}},
        )
        service = SimpleNamespace(runtime=runtime, conversation=ConversationLedger(runtime.store))
        loop = AgentLoop(service=service, model=FakeProvider(), tools=RuntimeToolAdapter(runtime))
        with patch.object(loop, "_account_response_usage"):
            for mode in ("native", "stream"):
                request = request_for(runtime, first.run_id, mode)
                wire = (loop.model.complete(request) if mode == "native"
                        else invoke_model_stream(loop, request))
                durable = loop._validate_response(request, wire)
                encoded = json.dumps(durable.to_dict())
                assert SECRET not in encoded and BUNDLE not in encoded
                arguments = durable.tool_calls[0]["arguments"]
                assert arguments["credential_ref"] == secret_reference(BUNDLE)
                call = ToolCall(call_id=mode, run_id=first.run_id,
                                tool_name="offline:cloud-inventory", arguments=arguments)
                result = loop.tools.invoke(call)
                assert result.status == "success"
                assert SECRET not in json.dumps(result.to_dict())
                record = runtime.store.model_responses(first.run_id)[-1]
                saved = ModelResponse.from_dict(record.response)
                assert record.response_hash == contract_hash(loop._response_projection(saved))
                expect_rejected(lambda: loop.tools.invoke(replace(call, run_id=second.run_id)),
                                "credential_reference_out_of_scope")

            request = request_for(runtime, second.run_id, "foreign-ref")
            foreign = replace(loop.model.complete(request), response_hash="", tool_calls=(
                {"id": "foreign", "name": "offline:cloud-inventory", "arguments": {
                    "credential_ref": secret_reference(BUNDLE)}},))
            expect_rejected(lambda: loop._validate_response(request, foreign),
                            "credential_reference_out_of_scope")
            assert not runtime.store.model_responses(second.run_id)

            request = request_for(runtime, second.run_id, "invalid-wire-hash")
            invalid = replace(loop.model.complete(request), response_hash="bad-hash")
            try:
                loop._validate_response(request, invalid)
            except ModelIntegrityError:
                pass
            else:
                raise AssertionError("wire hash was not validated")
            assert secret_reference(BUNDLE) not in runtime.tool_credential_refs(second.run_id)

        reconciled = loop.tools.reconcile(replace(call, idempotency_key="offline-reconcile"))
        assert reconciled is not None and reconciled.status == "success"
        assert SECRET not in json.dumps(reconciled.to_dict())

        for large, interrupted, omitted in ((False, True, False), (True, False, False), (True, True, False), (True, False, True)):
            identity = f"stream-{large}-{interrupted}-{omitted}"
            request = request_for(runtime, first.run_id, identity)
            text = json.dumps({"padding": "padding\n" * 10000 if large else "", "secret_access_key": "fresh complex secret \" escaped\nvalue"}, indent=2).replace('"secret_access_key": ', '"secret_access_key":\n')

            def stream(request):
                yield ModelStreamEvent(request_id=request.request_id, sequence=0,
                                       event_type="text_delta", payload={"delta": text})
                if interrupted:
                    raise RuntimeError(SECRET)
                yield ModelStreamEvent(request_id=request.request_id, sequence=1,
                                       event_type="completed", payload={"text": text})

            with patch.object(loop.model, "stream", stream), patch.object(loop, "_account_response_usage"), patch(
                "redteam_agent.application.model_stream.MAX_CREDENTIAL_PROJECTION_BYTES",
                1 if omitted else 16 * 1024 * 1024,
            ):
                try:
                    wire = invoke_model_stream(loop, request)
                except RuntimeError as error:
                    assert interrupted
                    loop._save_failure_response(request, error)
                else:
                    assert not interrupted
                    if omitted:
                        assert wire.metadata["text_incomplete"] is True
                        assert "incomplete_text_artifact" in wire.metadata
                        assert "complete_text_artifact" not in wire.metadata
                    loop._validate_response(request, wire)

        # Simulate process restart: durable scope remains but bindings must be supplied.
        runtime._credential_vault = CredentialVault()
        expect_rejected(lambda: loop.tools.invoke(call), "credential_reference_unbound")
        assert runtime.bind_credentials(first.run_id, {secret_reference(BUNDLE): BUNDLE}) == ()
        assert loop.tools.invoke(call).status == "success"
        expect_rejected(lambda: runtime.bind_credentials(second.run_id, {secret_reference(BUNDLE): BUNDLE}),
                        "credential_binding_reference_unknown")

        # Inspect actual persisted rows and journal bytes, not just API projections.
        for path in root.rglob("*"):
            if path.is_file():
                assert SECRET.encode() not in path.read_bytes(), str(path)
                assert b"fresh complex secret" not in path.read_bytes(), str(path)
        assert len(observed) == 4
        runtime.broker.close()
    print(json.dumps({"status": "passed", "checks": ["native_json_arguments", "split_stream_secret",
        "scoped_resolve", "cross_run_rejected", "raw_wire_hash", "durable_replay_hash",
        "restart_rebind", "tool_echo_projection", "reconcile_projection", "large_stream_artifact", "partial_stream_diagnostic", "projection_limit_omission", "disk_secret_absent"]}))


if __name__ == "__main__":
    main()
