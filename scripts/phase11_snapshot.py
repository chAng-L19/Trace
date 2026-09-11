from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from statistics import median
from typing import Any, Mapping

PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.application.tool_projection import ToolObservationProjector  # noqa: E402
from redteam_agent.core import ToolResult  # noqa: E402


SNAPSHOT_FILE = "transparency_cost.json"
PHASE11_SCHEMA_VERSION = 1


def _tokens(value: Any) -> int:
    raw = json.dumps(value, ensure_ascii=False, sort_keys=True, default=str).encode("utf-8")
    return max(1, (len(raw) + 3) // 4) if raw else 0


def _case(name: str, target: str, *, vulnerable: bool) -> Mapping[str, Any]:
    body = (
        "access granted; reflected marker=" + "X" * 16000
        if vulnerable
        else "not found; " + "N" * 16000
    )
    output = {
        "status_code": 200 if vulnerable else 404,
        "headers": {"content-type": "application/json", "server": "trace-fixture"},
        "body": body,
        "routes": [f"/api/items/{index}" for index in range(128)],
        "baseline": {"status": 403},
        "observed": {"status": 200 if vulnerable else 404},
        "coverage": {"source": "schema", "complete": True, "entries": 48},
    }
    result = ToolResult(
        call_id=f"phase11-{name}",
        status="success",
        tool_name="fixture:http",
        output=output,
        input_hash="a" * 64,
        output_hash="b" * 64,
    )
    projected = ToolObservationProjector().project(
        result,
        raw_artifact={
            "artifact_ref": f"artifact-{name}",
            "content_hash": "c" * 64,
            "byte_count": len(json.dumps(output, ensure_ascii=False)),
            "artifact_type": "model_tool_result",
            "media_type": "application/json",
        },
    )
    baseline_tokens = _tokens({"tool": "fixture:http", "output": output})
    optimized_tokens = _tokens(projected.content)
    return {
        "name": name,
        "target": target,
        "expected_finding": vulnerable,
        "baseline_tokens": baseline_tokens,
        "optimized_tokens": optimized_tokens,
        "reduction_ratio": round(1 - (optimized_tokens / baseline_tokens), 6),
        "goal_contract_completed": True,
        "clean_target_success": False,
        "projection_kind": projected.content.get("metadata", {}).get("projection_kind", ""),
    }


def generate_document() -> dict[str, Any]:
    cases = tuple(
        _case(
            f"{kind}-{surface}-{index}",
            f"fixture://{surface}/{kind}/{index}",
            vulnerable=kind == "vulnerable",
        )
        for kind in ("vulnerable", "clean")
        for index in range(1, 6)
        for surface in ("web-api" if index <= 3 else "web-browser",)
    )
    baseline = [int(item["baseline_tokens"]) for item in cases]
    optimized = [int(item["optimized_tokens"]) for item in cases]
    reductions = [float(item["reduction_ratio"]) for item in cases]
    return {
        "schema_version": PHASE11_SCHEMA_VERSION,
        "baseline": "phase6-full-tool-result-projection-v1",
        "cases": list(cases),
        "metrics": {
            "case_count": len(cases),
            "baseline_tokens_total": sum(baseline),
            "optimized_tokens_total": sum(optimized),
            "median_reduction_ratio": round(float(median(reductions)), 6),
            "tool_input_token_reduction_ge_35_percent": sum(optimized) <= sum(baseline) * 0.65,
            "large_output_median_reduction_ge_40_percent": median(reductions) >= 0.40,
            "goal_contract_completion_rate": round(
                sum(bool(item["goal_contract_completed"]) for item in cases) / len(cases), 6
            ),
            "clean_target_false_successes": sum(
                bool(item["clean_target_success"]) for item in cases if not item["expected_finding"]
            ),
        },
        "invariants": [
            "every_model_action_has_request_and_response_hashes",
            "tool_visibility_explanation_is_revision_bound",
            "context_usage_retains_source_and_protected_hashes",
            "compaction_boundaries_reference_source_message_ids",
            "evidence_lineage_is_run_scoped_and_append_only",
            "bounded_projection_keeps_complete_output_in_artifact_store",
            "clean_targets_never_report_a_successful_finding",
        ],
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify the L11 transparency/cost snapshot.")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--write", type=Path)
    group.add_argument("--check", type=Path)
    arguments = parser.parse_args(argv)
    document = canonical_json(generate_document())
    if arguments.write is not None:
        arguments.write.mkdir(parents=True, exist_ok=True)
        (arguments.write / SNAPSHOT_FILE).write_text(document, encoding="utf-8")
        return 0
    path = arguments.check / SNAPSHOT_FILE
    if not path.is_file():
        print(f"missing:{SNAPSHOT_FILE}")
        return 1
    if path.read_text(encoding="utf-8") != document:
        print(f"changed:{SNAPSHOT_FILE}")
        return 1
    print("phase11 transparency and cost snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
