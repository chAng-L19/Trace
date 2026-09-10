from __future__ import annotations

import codecs
import hashlib
import json
import tempfile
from pathlib import Path
from typing import Any, BinaryIO, ClassVar


class BoundedOutput:
    """Stream raw bytes to a file while keeping a bounded text projection."""

    DEFAULT_MAX_BYTES: ClassVar[int] = 32 * 1024
    DEFAULT_MAX_LINES: ClassVar[int] = 200
    CHUNK_BYTES: ClassVar[int] = 64 * 1024

    def __init__(
        self,
        *,
        max_bytes: int = DEFAULT_MAX_BYTES,
        max_lines: int = DEFAULT_MAX_LINES,
        path: Path | None = None,
        encoding: str = "utf-8",
    ) -> None:
        self.max_bytes = max(2, int(max_bytes))
        self.max_lines = max(1, int(max_lines))
        self._edge_bytes = max(1, self.max_bytes // 2)
        self._edge_lines = max(1, self.max_lines // 2)
        self._decoder = codecs.getincrementaldecoder(encoding)(errors="replace")
        self._handle: BinaryIO
        self._owned_path = path is None
        if path is None:
            handle = tempfile.NamedTemporaryFile(
                prefix="trace-bounded-output-", suffix=".bin", delete=False
            )
            self.path = Path(handle.name)
            self._handle = handle
        else:
            self.path = Path(path)
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self._handle = self.path.open("wb")
        self.byte_count = 0
        self._newline_count = 0
        self._saw_bytes = False
        self._last_byte = b""
        self._digest = hashlib.sha256()
        self._head = ""
        self._tail = ""
        self._closed = False

    @classmethod
    def capture_json(
        cls,
        value: Any,
        *,
        max_bytes: int = DEFAULT_MAX_BYTES,
        max_lines: int = DEFAULT_MAX_LINES,
    ) -> "BoundedOutput":
        output = cls(max_bytes=max_bytes, max_lines=max_lines)
        try:
            encoder = json.JSONEncoder(
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                default=str,
            )
            for piece in encoder.iterencode(value):
                output.append(piece)
        except BaseException:
            output.discard()
            raise
        return output

    @classmethod
    def preview_file(
        cls,
        path: Path,
        *,
        max_bytes: int = DEFAULT_MAX_BYTES,
        max_lines: int = DEFAULT_MAX_LINES,
    ) -> dict[str, Any]:
        output = cls(max_bytes=max_bytes, max_lines=max_lines)
        output._handle.close()
        output._closed = True
        try:
            with Path(path).open("rb") as source:
                for chunk in iter(lambda: source.read(cls.CHUNK_BYTES), b""):
                    output._observe(chunk)
            return output.preview()
        finally:
            output.discard()

    def append(self, value: Any) -> None:
        if self._closed:
            raise ValueError("bounded_output_closed")
        if isinstance(value, str):
            raw = value.encode("utf-8", errors="replace")
        elif isinstance(value, (bytes, bytearray, memoryview)):
            raw = bytes(value)
        else:
            raw = json.dumps(value, ensure_ascii=False, sort_keys=True, default=str).encode(
                "utf-8"
            )
        self._handle.write(raw)
        self._observe(raw)

    def _observe(self, raw: bytes) -> None:
        if not raw:
            return
        self._digest.update(raw)
        self.byte_count += len(raw)
        self._newline_count += raw.count(b"\n")
        self._saw_bytes = True
        self._last_byte = raw[-1:]
        decoded = self._decoder.decode(raw, final=False)
        if decoded:
            if self._head_bytes() < self._edge_bytes and self._head_lines() < self._edge_lines:
                self._head = self._trim_head(self._head + decoded)
            self._tail = self._trim_tail(self._tail + decoded)

    def _head_bytes(self) -> int:
        return len(self._head.encode("utf-8", errors="replace"))

    def _head_lines(self) -> int:
        return self._head.count("\n")

    def _trim_head(self, value: str) -> str:
        result: list[str] = []
        size = 0
        lines = 0
        for char in value:
            if lines >= self._edge_lines:
                break
            encoded = char.encode("utf-8", errors="replace")
            if size + len(encoded) > self._edge_bytes:
                break
            result.append(char)
            size += len(encoded)
            if char == "\n":
                lines += 1
        return "".join(result)

    def _trim_tail(self, value: str) -> str:
        result = value
        if result.count("\n") > self._edge_lines:
            result = "".join(result.splitlines(keepends=True)[-self._edge_lines :])
        encoded = result.encode("utf-8", errors="replace")
        if len(encoded) > self._edge_bytes:
            result = encoded[-self._edge_bytes :].decode("utf-8", errors="ignore")
        return result

    def close(self) -> None:
        if self._closed:
            return
        self._decoder.decode(b"", final=True)
        self._handle.flush()
        self._handle.close()
        self._closed = True

    def inline_text(self) -> str:
        self.close()
        return self.path.read_text(encoding="utf-8", errors="replace")

    def preview(self) -> dict[str, Any]:
        line_count = self._newline_count + int(self._saw_bytes and self._last_byte != b"\n")
        reasons: list[str] = []
        if self.byte_count > self.max_bytes:
            reasons.append("byte_limit")
        if line_count > self.max_lines:
            reasons.append("line_limit")
        return {
            "byte_count": self.byte_count,
            "line_count": line_count,
            "content_hash": self._digest.hexdigest(),
            "head": self._head,
            "tail": self._tail if self.byte_count > self._edge_bytes else "",
            "head_bytes": len(self._head.encode("utf-8", errors="replace")),
            "tail_bytes": len(self._tail.encode("utf-8", errors="replace")),
            "truncated": bool(reasons),
            "truncation_reason": reasons,
            "max_bytes": self.max_bytes,
            "max_lines": self.max_lines,
        }

    def discard(self) -> None:
        self.close()
        if self._owned_path:
            self.path.unlink(missing_ok=True)


__all__ = ["BoundedOutput"]
