from __future__ import annotations

import tempfile
from pathlib import Path
from typing import Any


class StreamTextAccumulator:
    def __init__(self) -> None:
        handle = tempfile.NamedTemporaryFile(prefix="redteam-model-stream-", suffix=".txt", delete=False)
        self.path = Path(handle.name)
        self._handle = handle
        self.byte_count = 0
        self._head = bytearray()
        self._tail = bytearray()

    def append(self, value: Any) -> None:
        raw = str(value).encode("utf-8", errors="replace")
        self._handle.write(raw)
        self.byte_count += len(raw)
        edge = 16 * 1024
        if len(self._head) < edge:
            self._head.extend(raw[: edge - len(self._head)])
        self._tail.extend(raw)
        if len(self._tail) > edge:
            del self._tail[:-edge]

    def close(self) -> None:
        if not self._handle.closed:
            self._handle.flush()
            self._handle.close()

    def inline_text(self) -> str:
        self.close()
        return self.path.read_text(encoding="utf-8", errors="replace")

    def preview(self) -> dict[str, Any]:
        return {
            "byte_count": self.byte_count,
            "head": bytes(self._head).decode("utf-8", errors="replace"),
            "tail": bytes(self._tail).decode("utf-8", errors="replace"),
            "truncated": self.byte_count > len(self._head) + len(self._tail),
        }

    def discard(self) -> None:
        self.close()
        self.path.unlink(missing_ok=True)
