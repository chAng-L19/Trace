from __future__ import annotations

import hashlib
import sqlite3
from dataclasses import replace
from pathlib import Path
from typing import Any, Iterable

from ..application.resources import ResourceIndex, ResourceResolver


def configured_resource_index(store: Any, resolver: ResourceResolver, roots: Iterable[Path | str]) -> ResourceIndex:
    index = resolver.index(roots)
    try:
        with store.connection() as connection:
            rows = connection.execute("SELECT skill_id,deleted,content_path FROM trace_skills").fetchall()
    except sqlite3.OperationalError as exc:
        if "no such table" not in str(exc).casefold():
            raise
        return index
    if not any(row["deleted"] or row["content_path"] for row in rows):
        return index
    overrides = {str(row["skill_id"]): row for row in rows}
    resources = {item.resource_id: item for item in index.resources
                 if item.resource_id not in overrides or not overrides[item.resource_id]["deleted"]}
    issues = list(index.issues)
    for skill_id, row in overrides.items():
        if row["deleted"] or not row["content_path"]:
            continue
        managed = resolver.index((Path(row["content_path"]),))
        issues.extend(managed.issues)
        if managed.resources:
            resources[skill_id] = replace(managed.resources[0], resource_id=skill_id, kind="skill", priority=40)
    ordered = tuple(sorted(resources.values(), key=lambda item: (-item.priority, item.source.casefold(), item.resource_id)))
    digest = hashlib.sha256(resolver._encode({"base_index_hash": index.index_hash,
                                             "resources": [item.to_dict() for item in ordered],
                                             "issues": [item.to_dict() for item in issues]})).hexdigest()
    return replace(index, resources=ordered, issues=tuple(issues), index_hash=digest)
