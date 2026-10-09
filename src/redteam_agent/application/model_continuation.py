from __future__ import annotations

from collections.abc import Mapping
from dataclasses import replace
import hashlib
from typing import Any

from ..core import contract_hash
from ..runtime.artifact_store import ArtifactIntegrityError


def binding(loop: Any, request: Any) -> dict[str, str]:
    capabilities = loop.model.capabilities()
    return {
        "branch_id": loop.service.journal.active_branch_id(request.run_id),
        "provider_scope": str(capabilities.metadata.get("continuation_scope") or
                              capabilities.metadata.get("provider") or type(loop.model).__name__),
        "model": request.model,
    }


def prepare_continuation(loop: Any, request: Any) -> Any:
    if not loop.model.capabilities().metadata.get("opaque_continuation"):
        return request
    bound = binding(loop, request)
    refs = []
    selected = {message.get("source_request_id") for message in request.messages
                if message.get("role") == "assistant"}
    for record in loop.service.journal.model_responses(request.run_id):
        ref = record.response.get("continuation")
        if record.status in {"completed", "success"} and isinstance(ref, Mapping):
            if record.request_id in selected and all(ref.get(key) == value for key, value in bound.items()):
                refs.append(dict(ref))
    return replace(request, continuation={"refs": refs, **bound} if refs else {},
                   metadata={**request.metadata, "continuation_binding": bound})


def hydrate_continuation(loop: Any, request: Any) -> Any:
    if not request.continuation:
        return request
    bound = binding(loop, request)
    chain = []
    for ref in request.continuation.get("refs", ()):
        if not all(ref.get(key) == value for key, value in bound.items()):
            raise loop._integrity_error("model_continuation_binding_mismatch")
        try:
            artifact = loop.service.runtime.artifacts.get_ref(ref["artifact_id"], run_id=request.run_id)
            if artifact is None or artifact.content_hash != ref.get("content_hash"):
                raise loop._integrity_error("model_continuation_hash_mismatch")
            if artifact.artifact_type != "provider_continuation" or any(
                artifact.metadata.get(key) != value for key, value in bound.items()
            ):
                raise loop._integrity_error("model_continuation_artifact_binding_mismatch")
            state = dict(loop.service.runtime.artifacts.read_json(artifact.artifact_id, run_id=request.run_id))
            identity = artifact.metadata.get("request_id")
            if state.get("assistant_request_id", identity) != identity:
                raise loop._integrity_error("model_continuation_request_binding_mismatch")
            state["assistant_request_id"] = identity
            chain.append(state)
        except ArtifactIntegrityError as error:
            raise loop._integrity_error("model_continuation_artifact_invalid") from error
    return replace(request, continuation={"chain": chain})


def persist_continuation(loop: Any, request: Any, response: Any) -> Any:
    if not response.continuation:
        return response
    from ..providers.opaque import opaque_only

    claimed = response.response_hash
    valid_claim = not claimed or claimed == contract_hash(loop._response_projection(response))
    continuation = opaque_only(response.continuation)
    if not loop.model.capabilities().metadata.get("opaque_continuation"):
        continuation = {}
    if continuation:
        runtime = loop.service.runtime
        runtime.project_model_credentials(request.run_id, response.to_dict())
        # A credential discovered in another response field also affects text on validation.
        projected_text = runtime.project_model_credentials(request.run_id, {"text": response.text})["text"]
        continuation["assistant_wire_text_hash"] = hashlib.sha256(response.text.encode()).hexdigest()
        continuation["assistant_projection_text_hash"] = hashlib.sha256(projected_text.encode()).hexdigest()
        if continuation.get("block_layout"):
            layout, texts, offset = [], [], 0
            for part in continuation["block_layout"]:
                part = dict(part)
                if part["type"] == "text":
                    length = int(part["length"])
                    text = runtime.project_model_credentials(request.run_id,
                        {"text": response.text[offset:offset + length]})["text"]
                    offset += length
                    part["length"] = len(text)
                    texts.append(text)
                layout.append(part)
            if offset == len(response.text) and "".join(texts) == projected_text:
                continuation["projection_block_layout"] = layout
            else:
                # Cross-block JSON normalization cannot preserve the provider's signed layout.
                continuation["projection_block_layout_unavailable"] = True
    ref = {}
    if continuation and response.status in {"completed", "success"}:
        bound = dict(request.metadata.get("continuation_binding") or binding(loop, request))
        artifact = loop.service.runtime.artifacts.put_json(
            continuation, run_id=request.run_id, artifact_type="provider_continuation",
            metadata={**bound, "request_id": request.request_id, "provider_private": True},
        )
        ref = {"artifact_id": artifact.artifact_id, "content_hash": artifact.content_hash,
               "provider_private": True, **bound}
    normalized = replace(response, continuation=ref)
    if claimed and valid_claim:
        normalized = replace(normalized, response_hash=contract_hash(loop._response_projection(normalized)))
    return normalized
