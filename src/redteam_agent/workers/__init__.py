import sys

from .local import LocalWorker
from . import manager as _manager
from .manager import CodexHandoffWorker, DockerWorkerAdapter, WorkerManager
from .mcp import McpWorker
from .workspace import RunWorkspace, WorkspaceManager

sys.modules[f"{__name__}.codex_handoff"] = _manager
sys.modules[f"{__name__}.docker"] = _manager

__all__ = [
    "CodexHandoffWorker",
    "DockerWorkerAdapter",
    "LocalWorker",
    "McpWorker",
    "RunWorkspace",
    "WorkerManager",
    "WorkspaceManager",
]
