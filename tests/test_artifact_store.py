from __future__ import annotations

from pathlib import Path

import pytest

from redteam_agent import AgentService, StartRequest
from redteam_agent.runtime.artifact_store import ArtifactIntegrityError
from redteam_agent.runtime.store_common import ImmutableRecordError


def _run(service: AgentService, session_id: str) -> str:
    return service.start(
        StartRequest(
            session_id=session_id,
            objective="Assess the supplied target and preserve complete worker artifacts",
        )
    ).single.run.run_id


def test_artifact_blob_deduplicates_while_references_remain_run_bound(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    first_run = _run(service, "artifact-first")
    second_run = _run(service, "artifact-second")
    store = service.runtime.artifacts

    first = store.put_bytes(
        b"same complete bytes",
        run_id=first_run,
        artifact_type="stdout",
        preview="same complete bytes",
    )
    repeated = store.put_bytes(
        b"same complete bytes",
        run_id=first_run,
        artifact_type="stdout",
        preview="same complete bytes",
    )
    second = store.put_bytes(
        b"same complete bytes",
        run_id=second_run,
        artifact_type="stdout",
        preview="same complete bytes",
    )

    assert repeated == first
    assert first.content_hash == second.content_hash
    assert first.artifact_id != second.artifact_id
    assert store.get_ref(first.artifact_id, run_id=second_run) is None
    with service.runtime.store.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM artifact_blobs").fetchone()[0] == 1
        assert connection.execute("SELECT COUNT(*) FROM artifact_refs").fetchone()[0] == 2


def test_artifact_same_content_can_have_multiple_semantic_refs_and_lineage(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "artifact-lineage")
    store = service.runtime.artifacts
    parent = store.put_json({"request": "GET /"}, run_id=run_id, artifact_type="request")
    response = store.put_json(
        {"request": "GET /"},
        run_id=run_id,
        artifact_type="response",
        parents=(parent.artifact_id,),
    )

    assert parent.content_hash == response.content_hash
    assert parent.artifact_id != response.artifact_id
    assert store.read_json(response.artifact_id, run_id=run_id) == {"request": "GET /"}

    other_run = _run(service, "artifact-other")
    with pytest.raises(ImmutableRecordError, match="artifact_parent_missing"):
        store.put_bytes(
            b"child",
            run_id=other_run,
            parents=(parent.artifact_id,),
        )


def test_artifact_detects_tampering_truncation_and_blob_metadata_corruption(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "artifact-integrity")
    store = service.runtime.artifacts
    ref = store.put_bytes(b"immutable complete output", run_id=run_id)
    path = store._path(ref.content_hash)

    path.write_bytes(b"truncated")
    with pytest.raises(ArtifactIntegrityError, match="artifact_integrity_mismatch"):
        store.read(ref.artifact_id, run_id=run_id)
    with pytest.raises(ArtifactIntegrityError, match="artifact_existing_hash_mismatch"):
        store.put_bytes(b"immutable complete output", run_id=run_id)


def test_artifact_projection_is_bounded_redacted_and_search_is_run_scoped(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    first_run = _run(service, "artifact-search-first")
    second_run = _run(service, "artifact-search-second")
    store = service.runtime.artifacts
    first = store.put_bytes(
        b"x" * 100_000,
        run_id=first_run,
        artifact_type="http_response",
        preview="needle-first " + "x" * 2048,
        metadata={"authorization": "Bearer secret-secret-secret", "route": "needle-first"},
    )
    store.put_bytes(
        b"other",
        run_id=second_run,
        artifact_type="http_response",
        preview="needle-second",
    )

    projection = store.project(first, max_preview_bytes=128)
    assert len(projection["preview"].encode("utf-8")) <= 128
    assert "secret-secret-secret" not in str(projection)
    assert [item.artifact_id for item in store.search(first_run, "needle-first")] == [
        first.artifact_id
    ]
    assert store.search(first_run, "needle-second") == ()


def test_artifact_metadata_and_preview_are_bounded_before_sqlite_persistence(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "artifact-bounds")
    store = service.runtime.artifacts
    ref = store.put_bytes(
        b"complete-cas-data",
        run_id=run_id,
        preview={"body": "p" * 500_000},
        metadata={"trace": "m" * 500_000},
    )

    assert ref.preview["truncated"] is True
    assert ref.metadata["truncated"] is True
    with service.runtime.store.connection() as connection:
        row = connection.execute(
            "SELECT length(preview_json), length(metadata_json), length(artifact_json) "
            "FROM artifact_refs WHERE artifact_id=?",
            (ref.artifact_id,),
        ).fetchone()
    assert row is not None
    assert int(row[0]) < 70_000
    assert int(row[1]) < 70_000
    assert int(row[2]) < 150_000
    assert store.read(ref.artifact_id, run_id=run_id) == b"complete-cas-data"


def test_artifact_cas_prefix_symlink_escape_is_rejected(tmp_path: Path) -> None:
    service = AgentService(root=tmp_path / "runtime")
    run_id = _run(service, "artifact-path")
    store = service.runtime.artifacts
    data = b"symlink escape probe"
    digest = store._digest(data)
    prefix = store.cas_root / digest[:2]
    outside = tmp_path / "outside"
    outside.mkdir()
    try:
        prefix.symlink_to(outside, target_is_directory=True)
    except (OSError, NotImplementedError):
        pytest.skip("directory symlink creation is unavailable")

    with pytest.raises(ArtifactIntegrityError, match="artifact_path_escape"):
        store.put_bytes(data, run_id=run_id)
