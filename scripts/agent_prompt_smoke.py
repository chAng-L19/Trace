"""Check isolated runtime instructions and durable sessions after a prompt update."""
from __future__ import annotations

import sys
from importlib.resources import files
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from model_recovery_smoke import response, scenario
from redteam_agent.application.agent_service import AgentService
from redteam_agent.application.execution_policy import TACTICAL_SYSTEM
from redteam_agent.core import contract_hash
from redteam_agent.providers import FakeModelProvider


def check_request(request):
    prompts = [item["content"]["system_invariant"] for item in request.messages
               if item["role"] == "system" and "system_invariant" in item["content"]]
    assert prompts == [TACTICAL_SYSTEM]
    assert "legacy-prompt-fixture" not in str(request.messages)
    assert "Pathological defensive programming" not in str(request.messages)


def main():
    prompt = files("redteam_agent").joinpath("prompts/agent.md").read_text(encoding="utf-8")
    assert TACTICAL_SYSTEM == prompt
    assert "## Trace Agent 行为准则" in prompt
    assert "## 工程原则" not in prompt
    waiting = response(structured_output={"decision": "waiting_input"})
    with scenario([waiting]) as (service, provider, run_id):
        old = service.conversation.append(
            run_id=run_id, role="system", content="legacy-prompt-fixture", protected=True,
            source_type="system_base", source_id="model-loop-v4")
        service.run(run_id)
        check_request(provider.requests[0])
        root = service.runtime.root
        service.close()
        resumed_provider = FakeModelProvider([waiting])
        resumed = AgentService(root=root, model_port=resumed_provider, load_external_configuration=False)
        try:
            resumed.context_selector.prepare_model_context(resumed.status(run_id), force_compaction=True)
            resumed.resume(run_id)
            check_request(resumed_provider.requests[0])
            bases = [item for item in resumed.conversation.messages(run_id)
                     if item.source_type == "system_base"]
            assert len(bases) == 2
            assert next(item for item in bases if item.message_id == old.message_id).content == old.content
            assert next(item for item in bases if item.content == prompt).source_id == (
                "model-loop-" + contract_hash(prompt))
        finally:
            resumed.close()
    print("agent prompt smoke: split, model request, compaction, restart, legacy history passed")


if __name__ == "__main__":
    main()
