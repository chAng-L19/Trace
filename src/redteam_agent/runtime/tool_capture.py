"""Private immutable captures and public, redacted evidence projections."""
from __future__ import annotations

import json
import threading
import urllib.parse
from typing import Any, Callable, Mapping

from .artifact_store import ArtifactStore
from .security import redact_sensitive


MAX_ENVELOPE_BYTES = 64 * 1024
MAX_CAPTURE_BYTES = 4 * 1024 * 1024
MAX_SCREENSHOT_BYTES = 16 * 1024 * 1024


class ToolCapture:
    def __init__(self, artifacts: ArtifactStore, run_id: str,
                 metadata: Mapping[str, Any], projector: Callable[[Any], Any]) -> None:
        self.artifacts, self.run_id = artifacts, run_id
        self.metadata, self.projector = dict(metadata), projector
        self._lock = threading.RLock()
        self._parts: dict[str, dict] = {}
        self.dispatched = False

    def _public(self, value: Any) -> Any:
        def urls(item):
            if isinstance(item, Mapping):
                return {key: safe_url(child) if key in {"url", "final_url", "requested_url"}
                        and isinstance(child, str) else urls(child) for key, child in item.items()}
            if isinstance(item, (list, tuple)):
                return [urls(child) for child in item]
            return item

        def safe_url(text):
            parsed = urllib.parse.urlsplit(text)
            if parsed.scheme not in {"http", "https"}:
                return text
            netloc = parsed.netloc.rsplit("@", 1)[-1]
            query = [(key, redact_sensitive(self.projector({key: child}))[key])
                     for key, child in urllib.parse.parse_qsl(parsed.query, keep_blank_values=True)]
            return urllib.parse.urlunsplit((parsed.scheme, netloc, parsed.path,
                                           urllib.parse.urlencode(query), ""))

        return redact_sensitive(self.projector(urls(value)))

    def _save(self, kind: str, raw: bytes, public: Any, *, media_type: str,
              truncated: bool = False) -> dict:
        with self._lock:
            private = self.artifacts.put_bytes(
                raw, run_id=self.run_id, artifact_type=kind + "_raw",
                media_type=media_type,
                metadata={**self.metadata, "provider_private": True,
                          "private_evidence": True, "capture_truncated": truncated},
            )
            projection = self.artifacts.put_json(
                self._public({"projection": public, "capture_truncated": truncated,
                              "raw_capture": {"private_artifact_ref": private.artifact_id,
                                              "content_hash": private.content_hash,
                                              "byte_count": private.byte_count,
                                              "access": "host_only"}}),
                run_id=self.run_id, artifact_type=kind,
                metadata={**self.metadata, "capture_projection": "redacted",
                          "capture_truncated": truncated},
            )
            part = {"artifact_ref": projection.artifact_id,
                    "content_hash": projection.content_hash,
                    "byte_count": projection.byte_count,
                    "raw_capture": {"private_artifact_ref": private.artifact_id,
                                    "content_hash": private.content_hash,
                                    "byte_count": private.byte_count,
                                    "access": "host_only"},
                    "capture_truncated": truncated}
            self._parts[kind] = part
            return part

    def envelope(self, kind: str, value: Mapping[str, Any]) -> dict:
        raw = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        if len(raw) > MAX_ENVELOPE_BYTES:
            raise ValueError("http_envelope_capture_limit")
        public = dict(value)
        public["capture_format"] = "client_envelope_not_wire"
        if "headers" in public:
            public["headers"] = [{"name": name, "value": self._public({name: item})[name]}
                                 for name, item in public["headers"]]
        return self._save(kind, raw, public, media_type="application/json")

    def body(self, kind: str, raw: bytes, *, content_type: str,
             truncated: bool = False) -> dict:
        if len(raw) > MAX_CAPTURE_BYTES:
            raise ValueError("http_body_capture_limit")
        public: dict[str, Any] = {"content_type": content_type, "text_available": False}
        try:
            text = raw.decode("utf-8", errors="strict")
            if "\x00" not in text:
                try:
                    decoded = json.loads(text)
                except ValueError:
                    decoded = text
                public.update(text_available=True, content=self._public(decoded))
        except UnicodeDecodeError:
            pass
        return self._save(kind, raw, public, media_type=content_type or "application/octet-stream",
                          truncated=truncated)

    def screenshot(self, raw: bytes) -> dict:
        if len(raw) > MAX_SCREENSHOT_BYTES:
            raise ValueError("browser_screenshot_capture_limit")
        dimensions = {}
        if raw.startswith(b"\x89PNG\r\n\x1a\n") and len(raw) >= 24:
            dimensions = {"width": int.from_bytes(raw[16:20], "big"),
                          "height": int.from_bytes(raw[20:24], "big")}
        return self._save("browser_screenshot", raw,
                          {"media_type": "image/png", **dimensions,
                           "visual_access": "host_only", "pixels_redacted": False},
                          media_type="image/png")

    def summary(self) -> dict:
        with self._lock:
            parts = dict(self._parts)
        return {"artifact_refs": [part["artifact_ref"] for part in parts.values()],
                "exchange_artifacts": parts,
                "capture_policy": "raw_host_only_public_redacted"}

    def project_output(self, output: Mapping[str, Any]) -> dict:
        projected = dict(output)
        if "header_items" in projected:
            projected["header_items"] = [
                {"name": name, "value": self._public({name: value})[name]}
                for name, value in projected["header_items"]
            ]
        projected = self._public(projected)
        if isinstance(projected.get("body"), str):
            projected["body_projection_truncated"] = len(projected["body"]) > 16 * 1024
            projected["body"] = projected["body"][:16 * 1024]
        projected.pop("screenshot_path", None)
        return projected
