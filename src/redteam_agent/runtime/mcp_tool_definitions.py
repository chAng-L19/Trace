from __future__ import annotations


_ALL_TOOL_DEFINITIONS = [
    {
        "name": "redteam_run",
        "description": "Single autonomous entrypoint: start or resume one operation or a multi-target batch, accept Host Agent observations, and continue to the next durable or terminal state.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "session_id": {"type": "string"},
                "objective": {"type": "string"},
                "targets": {"type": "array", "items": {"type": "string"}},
                "workflow_hint": {"type": "string"},
                "run_id": {"type": "string"},
                "batch_session_id": {"type": "string"},
                "observation": {
                    "type": "object",
                    "properties": {
                        "action_id": {"type": "string"},
                        "handoff_id": {"type": "string", "minLength": 1},
                        "handoff_token": {"type": "string", "minLength": 1},
                        "attempt_id": {"type": "string", "minLength": 1},
                        "contract_hash": {"type": "string", "minLength": 1},
                        "output": {},
                        "tool": {"type": "string"},
                        "usage": {
                            "type": "object",
                            "properties": {
                                "total_tokens": {"type": "integer", "minimum": 0},
                                "input_tokens": {"type": "integer", "minimum": 0},
                                "output_tokens": {"type": "integer", "minimum": 0},
                            },
                            "additionalProperties": False,
                        },
                    },
                    "required": ["handoff_id", "handoff_token", "attempt_id", "contract_hash", "output"],
                    "additionalProperties": False,
                },
                "observations": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "run_id": {"type": "string"},
                            "action_id": {"type": "string"},
                            "handoff_id": {"type": "string", "minLength": 1},
                            "handoff_token": {"type": "string", "minLength": 1},
                            "attempt_id": {"type": "string", "minLength": 1},
                            "contract_hash": {"type": "string", "minLength": 1},
                            "output": {},
                            "tool": {"type": "string"},
                            "usage": {
                                "type": "object",
                                "properties": {
                                    "total_tokens": {"type": "integer", "minimum": 0},
                                    "input_tokens": {"type": "integer", "minimum": 0},
                                    "output_tokens": {"type": "integer", "minimum": 0},
                                },
                                "additionalProperties": False,
                            },
                        },
                        "required": [
                            "run_id",
                            "handoff_id",
                            "handoff_token",
                            "attempt_id",
                            "contract_hash",
                            "output"
                        ],
                        "additionalProperties": False,
                    },
                },
                "starting_context": {"type": "object"},
                "constraints": {"type": "object"},
                "success_predicates": {"type": "array", "items": {"type": "object"}},
                "max_actions": {"type": "integer", "minimum": 1, "maximum": 512},
                "max_total_actions": {"type": "integer", "minimum": 1, "maximum": 4096},
                "max_tokens": {"type": "integer", "minimum": 1},
                "max_time_seconds": {"type": "number", "minimum": 0.1},
                "deadline": {"type": "string"},
                "budget_delta": {
                    "type": "object",
                    "properties": {
                        "actions": {"type": "integer", "minimum": 0},
                        "tokens": {"type": "integer", "minimum": 0},
                        "time_seconds": {"type": "number", "minimum": 0},
                        "deadline": {"type": "string"},
                        "acknowledge_missing_usage": {"type": "boolean"},
                    },
                    "additionalProperties": False,
                },
                "credential_bindings": {
                    "type": "object",
                    "additionalProperties": {"type": "string"},
                    "description": "Process-local mapping of required SECRET_REF values to raw tool-channel credentials.",
                },
                "auto_continue": {"type": "boolean"},
                "max_cycles": {"type": "integer", "minimum": 1, "maximum": 64},
                "max_retries_per_action": {"type": "integer", "minimum": 0, "maximum": 8},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_start",
        "description": "Compile a red-team goal, select a typed workflow, execute available tools, and persist the operation until its next durable state.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "session_id": {"type": "string"},
                "objective": {"type": "string"},
                "targets": {"type": "array", "items": {"type": "string"}},
                "workflow_hint": {"type": "string"},
                "starting_context": {"type": "object"},
                "constraints": {"type": "object"},
                "success_predicates": {"type": "array", "items": {"type": "object"}},
                "max_actions": {"type": "integer", "minimum": 1, "maximum": 512},
                "max_total_actions": {"type": "integer", "minimum": 1, "maximum": 4096},
                "max_tokens": {"type": "integer", "minimum": 1},
                "max_time_seconds": {"type": "number", "minimum": 0.1},
                "deadline": {"type": "string"},
                "max_retries_per_action": {"type": "integer", "minimum": 0, "maximum": 8},
            },
            "required": ["session_id", "objective"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_resume",
        "description": "Resume a persisted operation without requiring copied tool output from the user.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "max_actions": {"type": "integer", "minimum": 1, "maximum": 512},
                "credential_bindings": {
                    "type": "object",
                    "additionalProperties": {"type": "string"},
                },
            },
            "required": ["run_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_status",
        "description": "Return non-advancing operation or batch state, verified evidence, missing predicates, and the next executable action.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "batch_session_id": {"type": "string"},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_submit_observation",
        "description": "Submit host-agent tool output to the current typed action; semantic verification and lineage checks run before the workflow advances automatically.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "action_id": {"type": "string"},
                "output": {},
                "tool": {"type": "string"},
                "usage": {
                    "type": "object",
                    "properties": {
                        "total_tokens": {"type": "integer", "minimum": 0},
                        "input_tokens": {"type": "integer", "minimum": 0},
                        "output_tokens": {"type": "integer", "minimum": 0},
                    },
                    "additionalProperties": False,
                },
                "continue_run": {"type": "boolean"},
            },
            "required": ["run_id", "action_id", "output"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_evidence",
        "description": "Fetch one verified evidence node by operation and evidence ID when its payload was omitted from a compact status response.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "evidence_id": {"type": "string"},
            },
            "required": ["run_id", "evidence_id"],
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_cancel",
        "description": "Cancel an active operation, run an available cleanup action, and persist the cleanup outcome.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "batch_session_id": {"type": "string"},
                "reason": {"type": "string"},
            },
            "additionalProperties": False,
        },
    },
    {
        "name": "redteam_events",
        "description": "Return the durable event trace for an operation.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "run_id": {"type": "string"},
                "after_event_id": {"type": "integer", "minimum": 0},
                "limit": {"type": "integer", "minimum": 1, "maximum": 1000},
            },
            "required": ["run_id"],
            "additionalProperties": False,
        },
    },
]
PUBLIC_TOOL_NAMES = (
    "redteam_run",
    "redteam_status",
    "redteam_evidence",
    "redteam_cancel",
    "redteam_events",
)
LEGACY_TOOL_NAMES = (
    "redteam_start",
    "redteam_resume",
    "redteam_submit_observation",
)
TOOL_DEFINITIONS_BY_NAME = {str(item["name"]): item for item in _ALL_TOOL_DEFINITIONS}
TOOL_DEFINITIONS = [TOOL_DEFINITIONS_BY_NAME[name] for name in PUBLIC_TOOL_NAMES]
PUBLIC_TOOL_DEFINITIONS_BY_NAME = {
    name: TOOL_DEFINITIONS_BY_NAME[name] for name in PUBLIC_TOOL_NAMES
}
