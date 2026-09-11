from __future__ import annotations

import importlib
from pathlib import Path

from redteam_agent.runtime.workflow_registry import WorkflowRegistry


ROOT = Path(__file__).resolve().parents[1]


def test_deleted_modules_have_one_canonical_import_identity() -> None:
    aliases = {
        "redteam_agent.adapters.runtime_mapping": "redteam_agent.adapters.runtime",
        "redteam_agent.runtime.model_common": "redteam_agent.runtime.model_contracts",
        "redteam_agent.runtime.models": "redteam_agent.runtime.model_state",
        "redteam_agent.runtime.executor_common": "redteam_agent.runtime.plan",
        "redteam_agent.runtime.scheduler": "redteam_agent.runtime.plan",
        "redteam_agent.runtime.exploration_records": "redteam_agent.runtime.exploration",
        "redteam_agent.runtime.model_records": "redteam_agent.runtime.session_journal",
        "redteam_agent.runtime.store_common": "redteam_agent.runtime.security",
        "redteam_agent.runtime.store_schema": "redteam_agent.runtime.durable_store",
        "redteam_agent.runtime.service_store": "redteam_agent.runtime.durable_store",
        "redteam_agent.runtime.evidence_trust": "redteam_agent.runtime.evidence_gate",
        "redteam_agent.runtime.review": "redteam_agent.runtime.evidence_gate",
        "redteam_agent.runtime.operation_result": "redteam_agent.runtime.terminal_judge",
        "redteam_agent.core.domain.assets": "redteam_agent.core.domain",
        "redteam_agent.core.domain.goal": "redteam_agent.core.domain",
        "redteam_agent.core.domain.search": "redteam_agent.core.domain",
        "redteam_agent.core.ports.event": "redteam_agent.core.ports.model",
        "redteam_agent.core.ports.worker": "redteam_agent.core.ports.tool",
        "redteam_agent.workers.codex_handoff": "redteam_agent.workers.manager",
        "redteam_agent.workers.docker": "redteam_agent.workers.manager",
    }
    for legacy, canonical in aliases.items():
        assert importlib.import_module(legacy) is importlib.import_module(canonical)


def test_removed_files_are_not_reintroduced() -> None:
    removed = (
        "src/redteam_agent/adapters/runtime_mapping.py",
        "src/redteam_agent/application/lifecycle.py",
        "src/redteam_agent/application/model_integrity.py",
        "src/redteam_agent/application/stream_accumulator.py",
        "src/redteam_agent/core/domain/assets.py",
        "src/redteam_agent/core/domain/goal.py",
        "src/redteam_agent/core/domain/search.py",
        "src/redteam_agent/core/ports/event.py",
        "src/redteam_agent/core/ports/store.py",
        "src/redteam_agent/core/ports/worker.py",
        "src/redteam_agent/runtime/evidence_trust.py",
        "src/redteam_agent/runtime/executor_common.py",
        "src/redteam_agent/runtime/exploration_records.py",
        "src/redteam_agent/runtime/exploration_store.py",
        "src/redteam_agent/runtime/mcp_limits.py",
        "src/redteam_agent/runtime/model_common.py",
        "src/redteam_agent/runtime/model_records.py",
        "src/redteam_agent/runtime/models.py",
        "src/redteam_agent/runtime/review.py",
        "src/redteam_agent/runtime/scheduler.py",
        "src/redteam_agent/runtime/service_store.py",
        "src/redteam_agent/runtime/store_common.py",
        "src/redteam_agent/runtime/store_schema.py",
        "src/redteam_agent/workers/codex_handoff.py",
        "src/redteam_agent/workers/docker.py",
    )
    assert all(not (ROOT / path).exists() for path in removed)


def test_default_workflow_does_not_read_the_legacy_action_export(monkeypatch) -> None:
    original_read_text = Path.read_text

    def guarded_read_text(path: Path, *args, **kwargs):
        if path.name == "generic-adaptive.toml":
            raise AssertionError("legacy generic workflow export was read")
        return original_read_text(path, *args, **kwargs)

    monkeypatch.setattr(Path, "read_text", guarded_read_text)
    workflows = WorkflowRegistry().load()

    assert [workflow.workflow_id for workflow in workflows] == ["generic-adaptive"]
    assert workflows[0].version == 2
