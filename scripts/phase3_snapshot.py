from __future__ import annotations

import argparse
import inspect
import json
import sqlite3
import sys
import tempfile
from pathlib import Path
from typing import Any, Mapping


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.application import ModelLoop  # noqa: E402
from redteam_agent.core import ModelCapabilities, ModelRequest, ModelResponse, contract_hash  # noqa: E402
from redteam_agent.runtime.durable_store import DurableStore  # noqa: E402
from redteam_agent.runtime.model_records import (  # noqa: E402
    ModelObservationRecord,
    ModelRequestRecord,
    ModelResponseRecord,
)


SNAPSHOT_FILE = "model_loop.json"
MODEL_TABLES = (
    "model_requests",
    "model_responses",
    "model_stream_events",
    "model_observations",
)
PHASE3_SCHEMA_VERSION = 5


def _parameters(owner: Any, name: str) -> list[str]:
    signature = inspect.signature(getattr(owner, name))
    return [parameter for parameter in signature.parameters if parameter != "self"]


def _model_schema() -> dict[str, Any]:
    with tempfile.TemporaryDirectory(prefix="redteam-agent-phase3-") as directory:
        store = DurableStore(Path(directory))
        with store.connection() as connection:
            tables = {
                table: [
                    {
                        "name": str(row["name"]),
                        "type": str(row["type"]),
                        "not_null": bool(row["notnull"]),
                        "primary_key": int(row["pk"]),
                    }
                    for row in connection.execute(f"PRAGMA table_info({table})").fetchall()
                ]
                for table in MODEL_TABLES
            }
            indexes = [
                str(row["name"])
                for row in connection.execute(
                    "SELECT name FROM sqlite_master WHERE type='index' "
                    "AND (name LIKE 'idx_model_%' OR tbl_name LIKE 'model_%') "
                    "AND name NOT LIKE 'sqlite_%' ORDER BY name"
                ).fetchall()
            ]
    return {"version": PHASE3_SCHEMA_VERSION, "tables": tables, "indexes": indexes}


def generate_document() -> dict[str, Any]:
    capabilities = ModelCapabilities(
        native_system_role=True,
        native_tool_calls=True,
        parallel_tool_calls=True,
        structured_output=True,
        streaming=True,
        usage_reporting=True,
        max_context_tokens=128000,
        metadata={"provider": "fixture", "model": "fixture-primary"},
    )
    request = ModelRequest(
        request_id="model-request-phase3",
        run_id="run-phase3",
        messages=(
            {"role": "system", "content": "Runtime owns evidence and terminal decisions."},
            {"role": "user", "content": {"action_id": "map-surface"}},
        ),
        tools=(
            {
                "type": "function",
                "name": "fixture:inspect",
                "input_schema": {"type": "object"},
            },
        ),
        response_schema={"type": "object"},
        model="fixture-primary",
        allow_parallel_tools=True,
    )
    response = ModelResponse(
        request_id=request.request_id,
        status="completed",
        provider="fixture",
        model=request.model,
        tool_calls=(
            {"id": "call-phase3", "name": "fixture:inspect", "arguments": {}},
        ),
        usage={"input_tokens": 20, "output_tokens": 8, "total_tokens": 28},
        finish_reason="tool_calls",
    )
    prompt_projection = {
        "messages": [dict(item) for item in request.messages],
        "tools": [dict(item) for item in request.tools],
        "response_schema": dict(request.response_schema),
        "model": request.model,
        "allow_parallel_tools": request.allow_parallel_tools,
    }
    response_projection = response.to_dict()
    response_projection.pop("response_hash", None)
    request_record = ModelRequestRecord(
        request_id=request.request_id,
        run_id=request.run_id,
        prompt_hash=contract_hash(prompt_projection),
        provider="fixture",
        model=request.model,
        capabilities=capabilities.to_dict(),
        request=request.to_dict(),
        created_at="2026-08-11T00:00:00+00:00",
    )
    response_record = ModelResponseRecord(
        request_id=request.request_id,
        run_id=request.run_id,
        status="completed",
        provider="fixture",
        model=request.model,
        response_hash=contract_hash(response_projection),
        claimed_response_hash="",
        usage=response.usage,
        response=response.to_dict(),
        created_at="2026-08-11T00:00:01+00:00",
    )
    observation_record = ModelObservationRecord(
        observation_id="model-observation-phase3",
        request_id=request.request_id,
        run_id=request.run_id,
        action_id="map-surface",
        call_id="call-phase3",
        tool_name="fixture:inspect",
        status="success",
        input_hash="a" * 64,
        output_hash="b" * 64,
        observation={"tool_result": {"status": "success", "output": {"target": "fixture://phase3"}}},
        created_at="2026-08-11T00:00:02+00:00",
    )
    return {
        "model_loop": {
            "constructor": _parameters(ModelLoop, "__init__"),
            "methods": {
                "run": _parameters(ModelLoop, "run"),
                "cancel": _parameters(ModelLoop, "cancel"),
            },
        },
        "capabilities": capabilities.to_dict(),
        "records": {
            "request": request_record.to_dict(),
            "response": response_record.to_dict(),
            "observation": observation_record.to_dict(),
        },
        "schema": _model_schema(),
        "invariants": [
            "runtime_drives_model_loop",
            "provider_is_protocol_adapter_only",
            "runtime_computes_prompt_and_response_hashes",
            "tool_results_enter_model_observations_before_evidence",
            "model_text_cannot_write_evidence_or_terminal_state",
        ],
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify Phase 3 ModelLoop snapshot.")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", type=Path)
    group.add_argument("--check", type=Path)
    arguments = parser.parse_args(argv)
    document = generate_document()
    if arguments.write is not None:
        arguments.write.mkdir(parents=True, exist_ok=True)
        (arguments.write / SNAPSHOT_FILE).write_text(canonical_json(document), encoding="utf-8")
        return 0
    path = arguments.check / SNAPSHOT_FILE
    if not path.is_file():
        print(f"missing:{SNAPSHOT_FILE}")
        return 1
    if path.read_text(encoding="utf-8") != canonical_json(document):
        print(f"changed:{SNAPSHOT_FILE}")
        return 1
    print("phase3 ModelLoop snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
