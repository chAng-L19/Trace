import sys

from .model import Event, EventPort
from .model import (
    ModelCapabilities,
    ModelPort,
    ModelRequest,
    ModelResponse,
    ModelStreamEvent,
)
from typing import Protocol, runtime_checkable

from ..domain import Run


class StoreConflictError(RuntimeError):
    pass


@runtime_checkable
class StorePort(Protocol):
    def load_run(self, run_id: str) -> Run | None: ...

    def commit_run(self, run: Run, *, expected_version: int) -> Run: ...
from .tool import ToolCall, ToolDefinition, ToolPort, ToolResult
from .tool import WorkerPort, WorkerResult, WorkerTask

sys.modules[f"{__name__}.event"] = sys.modules[f"{__name__}.model"]
sys.modules[f"{__name__}.worker"] = sys.modules[f"{__name__}.tool"]

__all__ = [
    "Event",
    "EventPort",
    "ModelCapabilities",
    "ModelPort",
    "ModelRequest",
    "ModelResponse",
    "ModelStreamEvent",
    "StoreConflictError",
    "StorePort",
    "ToolCall",
    "ToolDefinition",
    "ToolPort",
    "ToolResult",
    "WorkerPort",
    "WorkerResult",
    "WorkerTask",
]
