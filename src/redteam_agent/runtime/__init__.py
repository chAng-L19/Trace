import sys

from . import model_contracts as _model_contracts
from . import model_state as _model_state
from . import security as _security

# Transitional module names resolve to canonical implementations for one
# migration cycle; they do not carry independent state or behavior.
sys.modules[f"{__name__}.model_common"] = _model_contracts
sys.modules[f"{__name__}.models"] = _model_state
sys.modules[f"{__name__}.store_common"] = _security

from . import evidence_gate as _evidence_gate

sys.modules[f"{__name__}.evidence_trust"] = _evidence_gate

from . import plan as _plan
from . import exploration as _exploration

sys.modules[f"{__name__}.executor_common"] = _plan
sys.modules[f"{__name__}.scheduler"] = _plan
sys.modules[f"{__name__}.exploration_records"] = _exploration

from .adaptive_planner import AdaptivePlanner
from .durable_store import (
    DurableStore,
    ImmutableRecordError,
    LeaseLostError,
    StateVersionConflict,
    StoreConflictError,
)
sys.modules[f"{__name__}.store_schema"] = sys.modules[f"{__name__}.durable_store"]
sys.modules[f"{__name__}.service_store"] = sys.modules[f"{__name__}.durable_store"]
from .evidence_graph import EvidenceGraph
from .evidence_gate import EvidenceGate, EvidenceGateDecision, GateEvaluationError, GateResult, ReviewEngine
from .exploration import ExplorationLedger, ExplorationValidationError, ReconDigestRecord, TacticalAttemptRecord
from .exploration_records import ReconDigestRecord, TacticalAttemptRecord
from .facts import FactLedger, FactValidationError
from .goal_compiler import GoalCompiler
from .intent_rewriter import PromptRewrite, REWRITE_VERSION, rewrite_objective
from .models import (
    ActionSpec,
    EvidenceNode,
    EvidenceProvenance,
    FactRecord,
    GatePredicate,
    GoalCriterion,
    GoalContract,
    LeaseToken,
    OperationState,
    RunBudget,
    ReviewRecord,
    SuccessPredicate,
    TaskAttempt,
    TerminalDecision,
    ToolCallResult,
    ToolDescriptor,
    WorkflowSpec,
)
from .operation_runtime import OperationResult, OperationRuntime
from .plan import (
    ExecutionOutcome,
    Fork,
    NextActionPolicy,
    NextActionProposal,
    PlanDelta,
    PlanFork,
    PlanRevision,
    PlanValidationError,
    ScheduleDecision,
    Scheduler,
)
from .session_journal import (
    JournalEntry,
    ModelObservationRecord,
    ModelRequestRecord,
    ModelResponseRecord,
    SessionJournal,
)
sys.modules[f"{__name__}.model_records"] = sys.modules[f"{__name__}.session_journal"]
from .store_migrations import MigrationReport, SchemaMigrationError
from .terminal_judge import TerminalJudge
sys.modules[f"{__name__}.operation_result"] = sys.modules[f"{__name__}.terminal_judge"]
sys.modules[f"{__name__}.review"] = sys.modules[f"{__name__}.evidence_gate"]
from .tool_broker import ToolBroker
from .tool_registry import ToolCatalog, ToolRegistry, ToolVisibility, ToolVisibilityPolicy
from .verifier import SemanticVerifier
from .workflow_registry import WorkflowRegistry

__all__ = [
    "ActionSpec",
    "AdaptivePlanner",
    "DurableStore",
    "ExecutionOutcome",
    "EvidenceGraph",
    "EvidenceGate",
    "EvidenceGateDecision",
    "ExplorationLedger",
    "ExplorationValidationError",
    "EvidenceNode",
    "EvidenceProvenance",
    "FactLedger",
    "FactRecord",
    "FactValidationError",
    "Fork",
    "GateEvaluationError",
    "GatePredicate",
    "GateResult",
    "GoalCompiler",
    "GoalCriterion",
    "GoalContract",
    "PromptRewrite",
    "REWRITE_VERSION",
    "ImmutableRecordError",
    "LeaseLostError",
    "LeaseToken",
    "JournalEntry",
    "MigrationReport",
    "NextActionPolicy",
    "NextActionProposal",
    "OperationResult",
    "OperationRuntime",
    "OperationState",
    "RunBudget",
    "PlanDelta",
    "PlanFork",
    "PlanRevision",
    "PlanValidationError",
    "ReviewEngine",
    "ReconDigestRecord",
    "ReviewRecord",
    "SchemaMigrationError",
    "SessionJournal",
    "SemanticVerifier",
    "StateVersionConflict",
    "StoreConflictError",
    "SuccessPredicate",
    "TaskAttempt",
    "TacticalAttemptRecord",
    "TerminalDecision",
    "TerminalJudge",
    "ToolBroker",
    "ToolCatalog",
    "ToolRegistry",
    "ToolVisibility",
    "ToolVisibilityPolicy",
    "ToolCallResult",
    "ToolDescriptor",
    "WorkflowRegistry",
    "WorkflowSpec",
    "rewrite_objective",
]
