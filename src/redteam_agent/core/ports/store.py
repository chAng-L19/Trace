from __future__ import annotations

from typing import Protocol, runtime_checkable

from ..domain import Run


class StoreConflictError(RuntimeError):
    pass


@runtime_checkable
class StorePort(Protocol):
    def load_run(self, run_id: str) -> Run | None: ...

    def commit_run(self, run: Run, *, expected_version: int) -> Run: ...
