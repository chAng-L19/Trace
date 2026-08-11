from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping

from ..core.contracts import json_mapping, required_text


@dataclass(frozen=True, slots=True)
class ModelRequestRecord:
    request_id: str
    run_id: str
    prompt_hash: str
    provider: str
    model: str
    capabilities: Mapping[str, Any]
    request: Mapping[str, Any]
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "request_id": self.request_id,
            "run_id": self.run_id,
            "prompt_hash": self.prompt_hash,
            "provider": self.provider,
            "model": self.model,
            "capabilities": dict(self.capabilities),
            "request": dict(self.request),
            "created_at": self.created_at,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelRequestRecord":
        return cls(
            request_id=required_text(payload.get("request_id"), "model_request_record_id"),
            run_id=required_text(payload.get("run_id"), "model_request_record_run_id"),
            prompt_hash=required_text(payload.get("prompt_hash"), "model_prompt_hash"),
            provider=required_text(payload.get("provider"), "model_provider"),
            model=str(payload.get("model") or ""),
            capabilities=json_mapping(payload.get("capabilities"), field="model_record.capabilities"),
            request=json_mapping(payload.get("request"), field="model_record.request"),
            created_at=required_text(payload.get("created_at"), "model_request_created_at"),
        )


@dataclass(frozen=True, slots=True)
class ModelResponseRecord:
    request_id: str
    run_id: str
    status: str
    provider: str
    model: str
    response_hash: str
    claimed_response_hash: str
    usage: Mapping[str, Any]
    response: Mapping[str, Any]
    created_at: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "request_id": self.request_id,
            "run_id": self.run_id,
            "status": self.status,
            "provider": self.provider,
            "model": self.model,
            "response_hash": self.response_hash,
            "claimed_response_hash": self.claimed_response_hash,
            "usage": dict(self.usage),
            "response": dict(self.response),
            "created_at": self.created_at,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelResponseRecord":
        return cls(
            request_id=required_text(payload.get("request_id"), "model_response_record_id"),
            run_id=required_text(payload.get("run_id"), "model_response_record_run_id"),
            status=required_text(payload.get("status"), "model_response_record_status"),
            provider=required_text(payload.get("provider"), "model_response_provider"),
            model=str(payload.get("model") or ""),
            response_hash=required_text(payload.get("response_hash"), "model_response_hash"),
            claimed_response_hash=str(payload.get("claimed_response_hash") or ""),
            usage=json_mapping(payload.get("usage"), field="model_response_record.usage"),
            response=json_mapping(payload.get("response"), field="model_response_record.response"),
            created_at=required_text(payload.get("created_at"), "model_response_created_at"),
        )


@dataclass(frozen=True, slots=True)
class ModelObservationRecord:
    observation_id: str
    request_id: str
    run_id: str
    action_id: str
    call_id: str
    tool_name: str
    status: str
    input_hash: str
    output_hash: str
    observation: Mapping[str, Any]
    created_at: str
    metadata: Mapping[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "observation_id": self.observation_id,
            "request_id": self.request_id,
            "run_id": self.run_id,
            "action_id": self.action_id,
            "call_id": self.call_id,
            "tool_name": self.tool_name,
            "status": self.status,
            "input_hash": self.input_hash,
            "output_hash": self.output_hash,
            "observation": dict(self.observation),
            "created_at": self.created_at,
            "metadata": dict(self.metadata),
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "ModelObservationRecord":
        return cls(
            observation_id=required_text(payload.get("observation_id"), "model_observation_id"),
            request_id=required_text(payload.get("request_id"), "model_observation_request_id"),
            run_id=required_text(payload.get("run_id"), "model_observation_run_id"),
            action_id=required_text(payload.get("action_id"), "model_observation_action_id"),
            call_id=required_text(payload.get("call_id"), "model_observation_call_id"),
            tool_name=required_text(payload.get("tool_name"), "model_observation_tool"),
            status=required_text(payload.get("status"), "model_observation_status"),
            input_hash=required_text(payload.get("input_hash"), "model_observation_input_hash"),
            output_hash=required_text(payload.get("output_hash"), "model_observation_output_hash"),
            observation=json_mapping(payload.get("observation"), field="model_observation.payload"),
            created_at=required_text(payload.get("created_at"), "model_observation_created_at"),
            metadata=json_mapping(payload.get("metadata"), field="model_observation.metadata"),
        )
