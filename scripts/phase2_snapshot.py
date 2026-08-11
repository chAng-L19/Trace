from __future__ import annotations

import argparse
import inspect
import json
import sys
from pathlib import Path
from typing import Any, Mapping


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.application import (  # noqa: E402
    ALLOWED_RUN_TRANSITIONS,
    AgentService,
    BudgetDelta,
    Observation,
    StartRequest,
)
from redteam_agent.adapters.runtime_mapping import LEGACY_TO_CORE_STATUS  # noqa: E402


SNAPSHOT_FILE = "agent_service.json"
PUBLIC_METHODS = ("start", "run", "submit_observation", "status", "cancel", "events")


def _parameters(name: str) -> list[str]:
    signature = inspect.signature(getattr(AgentService, name))
    return [parameter for parameter in signature.parameters if parameter != "self"]


def generate_document() -> dict[str, Any]:
    request = StartRequest.from_value(
        {
            "session_id": "phase2-snapshot",
            "objective": "Assess fixture://phase2 and report verified evidence",
            "targets": ["fixture://phase2"],
            "max_actions": 24,
            "token_limit": 100000,
            "time_limit_seconds": 1800,
        }
    )
    delta = BudgetDelta.from_value(
        {
            "actions": 4,
            "tokens": 2000,
            "time_seconds": 60,
            "idempotency_key": "phase2-budget-command",
        }
    )
    observation = Observation.from_value(
        {
            "action_id": "validate-path",
            "output": {"target": "fixture://phase2", "observed": "verified"},
            "tool": "fixture:validator",
            "idempotency_key": "phase2-observation-command",
            "continue_run": False,
        }
    )
    return {
        "service": {
            "class": "AgentService",
            "methods": {name: _parameters(name) for name in PUBLIC_METHODS},
        },
        "lifecycle": {
            "statuses": sorted(ALLOWED_RUN_TRANSITIONS),
            "transitions": {
                status: sorted(destinations)
                for status, destinations in sorted(ALLOWED_RUN_TRANSITIONS.items())
            },
            "legacy_projection": dict(sorted(LEGACY_TO_CORE_STATUS.items())),
        },
        "contracts": {
            "start_request": request.to_dict(),
            "budget_delta": delta.to_dict(),
            "observation": {
                "action_id": observation.action_id,
                "output": observation.output,
                "tool": observation.tool,
                "usage": dict(observation.usage),
                "idempotency_key": observation.idempotency_key,
                "continue_run": observation.continue_run,
                "max_actions": observation.max_actions,
                "has_handoff_receipt": observation.has_handoff_receipt,
            },
        },
    }


def canonical_json(value: Mapping[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate or verify Phase 2 AgentService snapshots.")
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
    print("phase2 AgentService snapshot verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
