from __future__ import annotations

import hashlib
import json
import os
import sqlite3
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping, Sequence

from .model_common import utc_now
from .security import redact_sensitive, secure_directory, secure_file
from .store_common import ImmutableRecordError, _dump, _load


ARTIFACT_SCHEMA_VERSION = 1
DEFAULT_INLINE_BYTES = 64 * 1024
DEFAULT_METADATA_BYTES = 64 * 1024


@dataclass(frozen=True, slots=True)
class ArtifactRef:
    """Immutable content-addressed artifact metadata."""

    artifact_id: str
    run_id: str
    content_hash: str
    byte_count: int
    media_type: str = "application/octet-stream"
    artifact_type: str = "tool_output"
    storage_key: str = ""
    preview: Any = None
    metadata: Mapping[str, Any] = field(default_factory=dict)
    created_at: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "artifact_id": self.artifact_id,
            "run_id": self.run_id,
            "content_hash": self.content_hash,
            "byte_count": self.byte_count,
            "media_type": self.media_type,
            "artifact_type": self.artifact_type,
            "storage_key": self.storage_key,
            "preview": self.preview,
            "metadata": dict(self.metadata or {}),
            "created_at": self.created_at,
        }


class ArtifactIntegrityError(RuntimeError):
    pass


class ArtifactStore:
    """SHA-256 CAS with SQLite metadata, run lineage and bounded projections."""

    def __init__(self, root: Path, store: Any) -> None:
        self.root = root
        self.store = store
        self.cas_root = root / "cas" / "sha256"
        secure_directory(self.cas_root)

    @staticmethod
    def _digest(data: bytes) -> str:
        return hashlib.sha256(data).hexdigest()

    def _path(self, digest: str) -> Path:
        if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
            raise ArtifactIntegrityError("artifact_hash_invalid")
        root = self.cas_root.resolve()
        path = self.cas_root / digest[:2] / digest[2:]
        if os.path.commonpath((str(root), str(path.parent.resolve()))) != str(root):
            raise ArtifactIntegrityError("artifact_path_escape")
        return path

    @staticmethod
    def _durable_projection(value: Any) -> Any:
        projected = redact_sensitive(value)
        return json.loads(
            json.dumps(projected, ensure_ascii=False, sort_keys=True, default=str)
        )

    @classmethod
    def _bounded_projection(cls, value: Any, *, max_bytes: int) -> Any:
        durable = cls._durable_projection(value)
        encoded = json.dumps(
            durable,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        bounded = max(256, int(max_bytes))
        if len(encoded) <= bounded:
            return durable
        edge = max(64, (bounded - 256) // 2)
        return {
            "truncated": True,
            "byte_count": len(encoded),
            "content_hash": hashlib.sha256(encoded).hexdigest(),
            "head": encoded[:edge].decode("utf-8", errors="replace"),
            "tail": encoded[-edge:].decode("utf-8", errors="replace"),
        }

    @staticmethod
    def _reference_id(
        *,
        run_id: str,
        content_hash: str,
        artifact_type: str,
        media_type: str,
        preview: Any,
        metadata: Mapping[str, Any],
        parents: Sequence[str],
    ) -> str:
        identity = json.dumps(
            {
                "run_id": run_id,
                "content_hash": content_hash,
                "artifact_type": artifact_type,
                "media_type": media_type,
                "preview": preview,
                "metadata": dict(metadata),
                "parents": list(parents),
            },
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return "artifact-" + hashlib.sha256(identity).hexdigest()[:32]

    def _write_blob(self, path: Path, raw: bytes, digest: str) -> None:
        secure_directory(path.parent)
        root = self.cas_root.resolve()
        parent = path.parent.resolve()
        if path.parent.is_symlink() or os.path.commonpath((str(root), str(parent))) != str(root):
            raise ArtifactIntegrityError("artifact_path_escape")
        if path.is_symlink():
            raise ArtifactIntegrityError("artifact_path_symlink")
        if path.exists():
            existing = path.read_bytes()
            if len(existing) != len(raw) or self._digest(existing) != digest:
                raise ArtifactIntegrityError("artifact_existing_hash_mismatch")
            return
        temporary = path.with_name(f".{path.name}.{os.getpid()}.{uuid.uuid4().hex}.tmp")
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        descriptor = -1
        try:
            descriptor = os.open(temporary, flags, 0o600)
            with os.fdopen(descriptor, "wb", closefd=True) as stream:
                descriptor = -1
                stream.write(raw)
                stream.flush()
                os.fsync(stream.fileno())
            secure_file(temporary)
            try:
                os.link(temporary, path)
            except FileExistsError:
                pass
            secure_file(path)
            self._verify_path(path, digest=digest, byte_count=len(raw))
        finally:
            if descriptor >= 0:
                os.close(descriptor)
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                pass

    def put_bytes(
        self,
        data: bytes,
        *,
        run_id: str,
        artifact_type: str = "tool_output",
        media_type: str = "application/octet-stream",
        preview: Any = None,
        metadata: Mapping[str, Any] | None = None,
        parents: Sequence[str] = (),
    ) -> ArtifactRef:
        raw = bytes(data)
        if self.store.load_operation(str(run_id)) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        digest = self._digest(raw)
        path = self._path(digest)
        self._write_blob(path, raw, digest)
        parent_ids = tuple(dict.fromkeys(str(item) for item in parents if str(item)))
        durable_preview = self._bounded_projection(preview, max_bytes=DEFAULT_INLINE_BYTES)
        durable_metadata = self._bounded_projection(
            dict(metadata or {}), max_bytes=DEFAULT_METADATA_BYTES
        )
        ref = ArtifactRef(
            artifact_id=self._reference_id(
                run_id=str(run_id),
                content_hash=digest,
                artifact_type=str(artifact_type),
                media_type=str(media_type),
                preview=durable_preview,
                metadata=durable_metadata,
                parents=parent_ids,
            ),
            run_id=str(run_id),
            content_hash=digest,
            byte_count=len(raw),
            media_type=str(media_type),
            artifact_type=str(artifact_type),
            storage_key=f"sha256/{digest[:2]}/{digest[2:]}",
            preview=durable_preview,
            metadata=durable_metadata,
            created_at=utc_now(),
        )
        return self._save_ref(ref, parent_ids)

    def put_json(
        self,
        value: Any,
        *,
        run_id: str,
        artifact_type: str = "tool_output",
        preview: Any = None,
        metadata: Mapping[str, Any] | None = None,
        parents: Sequence[str] = (),
    ) -> ArtifactRef:
        data = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str).encode("utf-8")
        return self.put_bytes(
            data,
            run_id=run_id,
            artifact_type=artifact_type,
            media_type="application/json",
            preview=preview,
            metadata=metadata,
            parents=parents,
        )

    def put_file(
        self,
        source: Path,
        *,
        run_id: str,
        artifact_type: str = "tool_output",
        media_type: str = "application/octet-stream",
        preview: Any = None,
        metadata: Mapping[str, Any] | None = None,
        parents: Sequence[str] = (),
    ) -> ArtifactRef:
        source_path = Path(source)
        if self.store.load_operation(str(run_id)) is None:
            raise KeyError(f"operation_not_found:{run_id}")
        if not source_path.is_file() or source_path.is_symlink():
            raise ArtifactIntegrityError("artifact_source_invalid")
        staging = self.root / f".artifact-stage-{os.getpid()}-{uuid.uuid4().hex}.tmp"
        digest_builder = hashlib.sha256()
        byte_count = 0
        try:
            with source_path.open("rb") as source_stream, staging.open("xb") as target_stream:
                for chunk in iter(lambda: source_stream.read(1024 * 1024), b""):
                    digest_builder.update(chunk)
                    byte_count += len(chunk)
                    target_stream.write(chunk)
                target_stream.flush()
                os.fsync(target_stream.fileno())
            secure_file(staging)
            digest = digest_builder.hexdigest()
            path = self._path(digest)
            secure_directory(path.parent)
            if path.is_symlink() or path.parent.is_symlink():
                raise ArtifactIntegrityError("artifact_path_symlink")
            if path.exists():
                self._verify_path(path, digest=digest, byte_count=byte_count)
            else:
                try:
                    # A hard link publishes without replacing an existing immutable CAS path.
                    os.link(staging, path)
                except FileExistsError:
                    self._verify_path(path, digest=digest, byte_count=byte_count)
                secure_file(path)
                self._verify_path(path, digest=digest, byte_count=byte_count)
        finally:
            staging.unlink(missing_ok=True)
        parent_ids = tuple(dict.fromkeys(str(item) for item in parents if str(item)))
        durable_preview = self._bounded_projection(preview, max_bytes=DEFAULT_INLINE_BYTES)
        durable_metadata = self._bounded_projection(
            dict(metadata or {}), max_bytes=DEFAULT_METADATA_BYTES
        )
        ref = ArtifactRef(
            artifact_id=self._reference_id(
                run_id=str(run_id),
                content_hash=digest,
                artifact_type=str(artifact_type),
                media_type=str(media_type),
                preview=durable_preview,
                metadata=durable_metadata,
                parents=parent_ids,
            ),
            run_id=str(run_id),
            content_hash=digest,
            byte_count=byte_count,
            media_type=str(media_type),
            artifact_type=str(artifact_type),
            storage_key=f"sha256/{digest[:2]}/{digest[2:]}",
            preview=durable_preview,
            metadata=durable_metadata,
            created_at=utc_now(),
        )
        return self._save_ref(ref, parent_ids)

    @staticmethod
    def _verify_path(path: Path, *, digest: str, byte_count: int) -> None:
        stored_hash = hashlib.sha256()
        stored_count = 0
        try:
            with path.open("rb") as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    stored_hash.update(chunk)
                    stored_count += len(chunk)
        except OSError as exc:
            raise ArtifactIntegrityError("artifact_write_verification_failed") from exc
        if stored_count != byte_count or stored_hash.hexdigest() != digest:
            raise ArtifactIntegrityError("artifact_write_verification_failed")

    def _save_ref(self, ref: ArtifactRef, parents: Sequence[str]) -> ArtifactRef:
        serialized = _dump(ref.to_dict())
        parent_ids = tuple(dict.fromkeys(str(item) for item in parents if str(item)))
        with self.store.transaction(immediate=True) as connection:
            blob = connection.execute(
                "SELECT content_hash, byte_count, storage_key FROM artifact_blobs WHERE content_hash=?",
                (ref.content_hash,),
            ).fetchone()
            if blob is None:
                connection.execute(
                    "INSERT INTO artifact_blobs(content_hash, byte_count, storage_key, created_at) VALUES(?, ?, ?, ?)",
                    (ref.content_hash, ref.byte_count, ref.storage_key, ref.created_at),
                )
            elif int(blob["byte_count"]) != ref.byte_count or str(blob["storage_key"]) != ref.storage_key:
                raise ArtifactIntegrityError(f"artifact_blob_metadata_mismatch:{ref.content_hash}")
            existing = connection.execute(
                "SELECT artifact_json FROM artifact_refs WHERE artifact_id=?", (ref.artifact_id,)
            ).fetchone()
            if existing is not None:
                payload = _load(existing["artifact_json"], None)
                saved = ArtifactRef(**payload) if isinstance(payload, Mapping) else None
                if saved is None or saved.to_dict() != {
                    **ref.to_dict(),
                    "created_at": saved.created_at if saved is not None else "",
                }:
                    raise ImmutableRecordError(f"immutable_artifact_conflict:{ref.artifact_id}")
                saved_parents = tuple(
                    str(row["parent_id"])
                    for row in connection.execute(
                        "SELECT parent_id FROM artifact_links WHERE artifact_id=? ORDER BY parent_id",
                        (ref.artifact_id,),
                    ).fetchall()
                )
                if saved_parents != tuple(sorted(parent_ids)):
                    raise ImmutableRecordError(f"immutable_artifact_lineage:{ref.artifact_id}")
                return saved
            else:
                connection.execute(
                    "INSERT INTO artifact_refs(artifact_id, run_id, content_hash, byte_count, media_type, artifact_type, storage_key, preview_json, metadata_json, artifact_json, created_at) "
                    "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        ref.artifact_id,
                        ref.run_id,
                        ref.content_hash,
                        ref.byte_count,
                        ref.media_type,
                        ref.artifact_type,
                        ref.storage_key,
                        _dump(ref.preview),
                        _dump(dict(ref.metadata or {})),
                        serialized,
                        ref.created_at,
                    ),
                )
                connection.execute(
                    "INSERT INTO artifact_fts(artifact_id, run_id, artifact_type, preview, metadata) VALUES(?, ?, ?, ?, ?)",
                    (
                        ref.artifact_id,
                        ref.run_id,
                        ref.artifact_type,
                        _dump(ref.preview),
                        _dump(dict(ref.metadata or {})),
                    ),
                )
            for parent_id in parent_ids:
                parent = connection.execute(
                    "SELECT 1 FROM artifact_refs WHERE artifact_id=? AND run_id=?", (parent_id, ref.run_id)
                ).fetchone()
                if parent is None:
                    raise ImmutableRecordError(f"artifact_parent_missing:{parent_id}")
                connection.execute(
                    "INSERT OR IGNORE INTO artifact_links(artifact_id, parent_id, run_id) VALUES(?, ?, ?)",
                    (ref.artifact_id, parent_id, ref.run_id),
                )
        return ref

    def get_ref(self, artifact_id: str, *, run_id: str) -> ArtifactRef | None:
        with self.store.connection() as connection:
            row = connection.execute(
                "SELECT * FROM artifact_refs WHERE artifact_id=? AND run_id=?",
                (artifact_id, run_id),
            ).fetchone()
        if row is None:
            return None
        payload = _load(row["artifact_json"], None)
        if not isinstance(payload, Mapping):
            raise ArtifactIntegrityError(f"artifact_reference_corrupt:{artifact_id}")
        try:
            ref = ArtifactRef(**payload)
        except (TypeError, ValueError) as exc:
            raise ArtifactIntegrityError(f"artifact_reference_corrupt:{artifact_id}") from exc
        if (
            ref.artifact_id != str(row["artifact_id"])
            or ref.run_id != str(row["run_id"])
            or ref.content_hash != str(row["content_hash"])
            or ref.byte_count != int(row["byte_count"])
            or ref.media_type != str(row["media_type"])
            or ref.artifact_type != str(row["artifact_type"])
            or ref.storage_key != str(row["storage_key"])
            or _dump(ref.preview) != str(row["preview_json"])
            or _dump(dict(ref.metadata)) != str(row["metadata_json"])
            or ref.created_at != str(row["created_at"])
        ):
            raise ArtifactIntegrityError(f"artifact_reference_integrity:{artifact_id}")
        with self.store.connection() as connection:
            blob = connection.execute(
                "SELECT byte_count, storage_key FROM artifact_blobs WHERE content_hash=?",
                (ref.content_hash,),
            ).fetchone()
            links = tuple(
                str(item["parent_id"])
                for item in connection.execute(
                    "SELECT parent_id FROM artifact_links WHERE artifact_id=? AND run_id=? ORDER BY parent_id",
                    (artifact_id, run_id),
                ).fetchall()
            )
        if (
            blob is None
            or int(blob["byte_count"]) != ref.byte_count
            or str(blob["storage_key"]) != ref.storage_key
        ):
            raise ArtifactIntegrityError(f"artifact_blob_integrity:{artifact_id}")
        expected_id = self._reference_id(
            run_id=ref.run_id,
            content_hash=ref.content_hash,
            artifact_type=ref.artifact_type,
            media_type=ref.media_type,
            preview=ref.preview,
            metadata=ref.metadata,
            parents=links,
        )
        if expected_id != artifact_id:
            raise ArtifactIntegrityError(f"artifact_lineage_integrity:{artifact_id}")
        return ref

    def read(self, artifact_id: str, *, run_id: str) -> bytes:
        ref = self.get_ref(artifact_id, run_id=run_id)
        if ref is None:
            raise KeyError(f"artifact_not_found:{artifact_id}")
        path = self._path(ref.content_hash)
        try:
            data = path.read_bytes()
        except OSError as exc:
            raise ArtifactIntegrityError(f"artifact_missing:{artifact_id}") from exc
        if len(data) != ref.byte_count or self._digest(data) != ref.content_hash:
            raise ArtifactIntegrityError(f"artifact_integrity_mismatch:{artifact_id}")
        return data

    def read_json(self, artifact_id: str, *, run_id: str) -> Any:
        return json.loads(self.read(artifact_id, run_id=run_id).decode("utf-8"))

    def verify(self, artifact_id: str, *, run_id: str) -> bool:
        self.read(artifact_id, run_id=run_id)
        return True

    def refs(self, run_id: str) -> tuple[ArtifactRef, ...]:
        with self.store.connection() as connection:
            rows = connection.execute(
                "SELECT artifact_id FROM artifact_refs WHERE run_id=? ORDER BY created_at, artifact_id", (run_id,)
            ).fetchall()
        return tuple(self._required_ref(str(row["artifact_id"]), run_id=run_id) for row in rows)

    def _required_ref(self, artifact_id: str, *, run_id: str) -> ArtifactRef:
        ref = self.get_ref(artifact_id, run_id=run_id)
        if ref is None:
            raise ArtifactIntegrityError(f"artifact_reference_missing:{artifact_id}")
        return ref

    def search(self, run_id: str, query: str, *, limit: int = 20) -> tuple[ArtifactRef, ...]:
        bounded = max(1, min(100, int(limit)))
        phrase = str(query).strip()[:1024]
        if not phrase:
            return ()
        literal = '"' + phrase.replace('"', '""') + '"'
        with self.store.connection() as connection:
            rows = connection.execute(
                "SELECT a.artifact_id, f.artifact_type AS fts_type, f.preview AS fts_preview, "
                "f.metadata AS fts_metadata FROM artifact_fts f "
                "JOIN artifact_refs a ON a.artifact_id=f.artifact_id "
                "WHERE f.run_id=? AND a.run_id=? AND artifact_fts MATCH ? ORDER BY a.created_at LIMIT ?",
                (run_id, run_id, literal, bounded),
            ).fetchall()
        results: list[ArtifactRef] = []
        for row in rows:
            ref = self._required_ref(str(row["artifact_id"]), run_id=run_id)
            if (
                str(row["fts_type"]) != ref.artifact_type
                or str(row["fts_preview"]) != _dump(ref.preview)
                or str(row["fts_metadata"]) != _dump(dict(ref.metadata))
            ):
                raise ArtifactIntegrityError(f"artifact_fts_integrity:{ref.artifact_id}")
            results.append(ref)
        return tuple(results)

    @staticmethod
    def project(ref: ArtifactRef, *, max_preview_bytes: int = DEFAULT_INLINE_BYTES) -> dict[str, Any]:
        preview = ref.preview
        if preview is not None:
            serialized = json.dumps(preview, ensure_ascii=False, sort_keys=True, default=str)
            if len(serialized.encode("utf-8")) > max_preview_bytes:
                preview = serialized.encode("utf-8")[:max_preview_bytes].decode("utf-8", errors="ignore")
        return {
            "artifact_ref": ref.artifact_id,
            "content_hash": ref.content_hash,
            "byte_count": ref.byte_count,
            "media_type": ref.media_type,
            "artifact_type": ref.artifact_type,
            "preview": preview,
            "metadata": dict(ref.metadata or {}),
        }
