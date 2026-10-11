"""User-facing workspace projections over Trace's existing run ledger.

Workspaces are an organization layer, not a second evidence database. Runs,
goals, exploration records, evidence and worker tasks remain authoritative.
Workspace metadata and intent leases live here; conclusions use the fact ledger.
"""
from __future__ import annotations

import copy
from contextlib import contextmanager
from datetime import datetime, timezone
import json
from typing import TYPE_CHECKING, Any, Mapping
from uuid import uuid4

from ..runtime.model_state import FactRecord

if TYPE_CHECKING:
    from .agent_service import AgentService


TERMINAL = {"completed", "failed", "failed_integrity", "cancelled"}
# Batch children retain their Runtime session identities; the existing goal
# context is the workspace association for both single and multi-target runs.
_WORKSPACE_ID = "COALESCE(NULLIF(json_extract(state_json, '$.goal.starting_context.workspace_id'), ''), session_id)"


def _now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


class WorkspaceManager:
    def __init__(self, service: "AgentService") -> None:
        self.service = service
        self.store = service.runtime.store

    @contextmanager
    def _creation_transaction(self):
        """Bind only creation's collaborators to one transaction, as in discussion.

        Store methods normally open their own connections. Private copies let
        start(), its journal writes and workspace rows commit together without
        changing the shared service/store seen by other threads.
        """
        with self.service._write_scope(), self.service._model_lock, self.store.transaction(immediate=True) as connection:
            @contextmanager
            def borrowed(**kwargs):
                yield connection

            store = copy.copy(self.store)
            store.connection = store.transaction = borrowed
            service = copy.copy(self.service)
            service.runtime = copy.copy(self.service.runtime)
            service.runtime.store = store
            service.runtime.evidence_graph = copy.copy(self.service.runtime.evidence_graph)
            service.runtime.evidence_graph.store = store
            service.runtime.executor = copy.copy(self.service.runtime.executor)
            service.runtime.executor.store = store
            service.runtime.executor.evidence_graph = service.runtime.evidence_graph
            service.conversation = copy.copy(self.service.conversation)
            service.conversation.store = store
            manager = copy.copy(self)
            manager.service, manager.store = service, store
            yield manager

    def _meta(self, workspace_id: str) -> Mapping[str, Any] | None:
        with self.store.connection() as connection:
            row = connection.execute(
                "SELECT * FROM workspace_meta WHERE workspace_id=?", (workspace_id,)
            ).fetchone()
        return dict(row) if row is not None else None

    def _run_ids(self, workspace_id: str) -> tuple[str, ...]:
        with self.store.connection() as connection:
            rows = connection.execute(
                f"SELECT run_id FROM operations WHERE {_WORKSPACE_ID}=? ORDER BY updated_at DESC, run_id DESC",
                (workspace_id,),
            ).fetchall()
        return tuple(str(row["run_id"]) for row in rows)

    def _workspace_ids(self) -> tuple[str, ...]:
        with self.store.connection() as connection:
            rows = connection.execute(
                f"SELECT workspace_id FROM workspace_meta UNION SELECT DISTINCT {_WORKSPACE_ID} FROM operations"
            ).fetchall()
        return tuple(sorted({str(row[0]) for row in rows if str(row[0]).strip()}))

    def _run_summary(self, run_id: str) -> dict[str, Any]:
        state = self.store.load_operation(run_id)
        if state is None:
            raise KeyError(f"operation_not_found:{run_id}")
        budget = state.budget.to_dict()
        return {
            "run_id": run_id,
            "session_id": state.session_id,
            "status": state.status,
            "objective": state.goal.objective,
            "targets": list(state.goal.targets),
            "created_at": state.created_at,
            "updated_at": state.updated_at,
            "actions_used": budget.get("actions_used", 0),
            "action_limit": budget.get("action_limit", 0),
            "pause_reason": budget.get("pause_reason", ""),
            "next_action": state.current_action_id,
        }

    def _status(self, workspace_id: str, run_ids: tuple[str, ...]) -> str:
        meta = self._meta(workspace_id)
        if meta is not None:
            return str(meta["status"])
        states = [self.store.load_operation(run_id) for run_id in run_ids]
        states = [state for state in states if state is not None]
        if any(state.status not in TERMINAL and state.status != "paused_budget" for state in states):
            return "active"
        if states and all(state.status == "completed" for state in states):
            return "completed"
        return "stopped"

    def _summary(self, workspace_id: str) -> dict[str, Any]:
        self._expire_leases(workspace_id)
        meta = self._meta(workspace_id) or {}
        run_ids = self._run_ids(workspace_id)
        runs = [self._run_summary(run_id) for run_id in run_ids]
        latest = runs[0] if runs else {}
        title = str(meta.get("title") or latest.get("objective") or workspace_id)
        status = self._status(workspace_id, run_ids)
        with self.store.connection() as connection:
            hint_count = int(connection.execute(
                "SELECT COUNT(*) FROM workspace_hints WHERE workspace_id=?", (workspace_id,)
            ).fetchone()[0])
            worker_count = int(connection.execute(
                f"SELECT COUNT(*) FROM worker_tasks WHERE run_id IN (SELECT run_id FROM operations WHERE {_WORKSPACE_ID}=?)",
                (workspace_id,),
            ).fetchone()[0])
            intent_row = connection.execute(
                "SELECT COUNT(*) AS total, "
                "SUM(CASE WHEN concluded_at='' THEN 1 ELSE 0 END) AS open_count, "
                "SUM(CASE WHEN concluded_at='' AND worker!='' THEN 1 ELSE 0 END) AS working_count, "
                "SUM(CASE WHEN concluded_at='' AND worker='' THEN 1 ELSE 0 END) AS unclaimed_count "
                "FROM workspace_intents WHERE workspace_id=?", (workspace_id,)
            ).fetchone()
            legacy_count = connection.execute(
                f"SELECT COUNT(*) FROM operations o WHERE {_WORKSPACE_ID}=? AND NOT EXISTS "
                "(SELECT 1 FROM workspace_intents i WHERE i.run_id=o.run_id)", (workspace_id,)
            ).fetchone()[0]
        return {
            "workspace_id": workspace_id,
            "title": title,
            "status": status,
            "origin": str(meta.get("origin") or (latest.get("targets") or [""])[0]),
            "goal": str(meta.get("goal") or latest.get("objective") or ""),
            "bootstrap_enabled": bool(meta.get("bootstrap_enabled", 1)),
            "created_at": str(meta.get("created_at") or latest.get("created_at") or ""),
            "updated_at": max(str(meta.get("updated_at") or ""), str(latest.get("updated_at") or "")),
            "run_count": len(runs),
            "active_run_count": sum(item["status"] not in TERMINAL and item["status"] != "paused_budget" for item in runs),
            "hint_count": hint_count,
            "intent_count": int(intent_row["total"] or 0) + legacy_count,
            "open_intent_count": int(intent_row["open_count"] or 0) + legacy_count,
            "working_intent_count": int(intent_row["working_count"] or 0),
            "unclaimed_intent_count": int(intent_row["unclaimed_count"] or 0) + legacy_count,
            "worker_count": worker_count,
            "reason_worker": str(meta.get("reason_worker") or ""),
            "reason_trigger": str(meta.get("reason_trigger") or ""),
            "reason_last_heartbeat_at": str(meta.get("reason_last_heartbeat_at") or ""),
            "latest_run_id": str(latest.get("run_id") or ""),
        }

    def list(self, *, search: str = "", status: str = "", limit: int = 100, offset: int = 0) -> dict[str, Any]:
        values = [self._summary(workspace_id) for workspace_id in self._workspace_ids()]
        keyword = str(search or "").casefold().strip()
        if keyword:
            values = [item for item in values if keyword in " ".join(
                str(item.get(key, "")) for key in ("workspace_id", "title", "origin", "goal")
            ).casefold()]
        if status:
            values = [item for item in values if item["status"] == status]
        values.sort(key=lambda item: (item["updated_at"], item["workspace_id"]), reverse=True)
        bounded = max(1, min(1000, int(limit)))
        start = max(0, int(offset))
        page = values[start:start + bounded]
        return {"workspaces": page, "truncated": start + bounded < len(values),
                "next_offset": start + len(page) if start + bounded < len(values) else None}

    def detail(self, workspace_id: str) -> dict[str, Any]:
        self._require(workspace_id)
        summary = self._summary(workspace_id)
        run_ids = self._run_ids(workspace_id)
        runs = [self._run_summary(run_id) for run_id in run_ids]
        meta = self._meta(workspace_id) or {}
        with self.store.connection() as connection:
            hints = [dict(row) for row in connection.execute(
                "SELECT hint_id, content, creator, created_at FROM workspace_hints WHERE workspace_id=? ORDER BY created_at, hint_id",
                (workspace_id,),
            ).fetchall()]
        facts = [
            {"id": "origin", "description": summary["origin"], "source": "workspace"},
            {"id": "goal", "description": summary["goal"], "source": "workspace"},
        ]
        with self.store.connection() as connection:
            intent_rows = connection.execute(
                "SELECT * FROM workspace_intents WHERE workspace_id=? ORDER BY created_at, intent_id",
                (workspace_id,),
            ).fetchall()
        intents = []
        known_runs = set()
        for row in intent_rows:
            run_id = str(row["run_id"])
            known_runs.add(run_id)
            intents.append({
                "id": str(row["intent_id"]), "run_id": run_id,
                "from": json.loads(str(row["from_json"] or "[]")),
                "to": str(row["to_fact"] or "") or None,
                "description": str(row["description"]), "creator": str(row["creator"]),
                "worker": str(row["worker"] or "") or None,
                "last_heartbeat_at": str(row["last_heartbeat_at"] or "") or None,
                "status": "concluded" if str(row["concluded_at"] or "") else "open",
                "created_at": str(row["created_at"]),
                "concluded_at": str(row["concluded_at"] or "") or None,
            })
        for item in runs:
            if item["run_id"] not in known_runs:
                intents.append({
                    "id": "run:" + item["run_id"], "run_id": item["run_id"], "from": [], "to": None,
                    "description": item["objective"], "creator": "operator", "worker": None,
                    "last_heartbeat_at": None,
                    "status": "open", "legacy": True,
                    "created_at": item["created_at"],
                    "concluded_at": None,
                })
            for fact in self.store.facts(item["run_id"]):
                if fact.key.startswith("workspace_intent:"):
                    facts.append({"id": fact.fact_id, **fact.value, "run_id": item["run_id"],
                                  "created_at": fact.created_at})
            try:
                evidence = self.service.runtime.evidence_graph.list(item["run_id"], include_unverified=False)
            except KeyError:
                evidence = ()
            for node in evidence:
                payload = node.to_dict()
                facts.append({
                    "id": str(payload.get("evidence_id") or ""),
                    "description": str(payload.get("artifact_type") or payload.get("tool") or "已验证证据"),
                    "source": "evidence", "run_id": item["run_id"],
                })
        return {"workspace": summary, "facts": facts, "intents": intents,
                "hints": hints, "runs": runs, "meta": dict(meta)}

    def create(self, payload: Mapping[str, Any]) -> dict[str, Any]:
        workspace_id = str(payload.get("workspace_id") or "ws-" + uuid4().hex[:16]).strip()
        title = str(payload.get("title") or "未命名工作区").strip()
        origin = str(payload.get("origin") or "").strip()
        goal = str(payload.get("goal") or title).strip()
        if not workspace_id or not title or not goal:
            raise ValueError("workspace_title_goal_required")
        now = _now()
        with self._creation_transaction() as manager:
            with manager.store.transaction() as connection:
                connection.execute(
                    "INSERT INTO workspace_meta(workspace_id,title,status,origin,goal,bootstrap_enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)",
                    (workspace_id, title, "active", origin, goal, int(bool(payload.get("bootstrap_enabled", True))), now, now),
                )
            started = manager.service.start({
                "session_id": workspace_id, "objective": goal,
                "targets": list(payload.get("targets") or ([origin] if origin else [])),
                "max_actions": int(payload.get("max_actions", 0)),
                "starting_context": {"workspace_id": workspace_id},
            })
            for view in started.runs:
                manager._ensure_intent(workspace_id, view.run.run_id, goal)
            for hint in payload.get("hints") or ():
                manager.add_hint(workspace_id, hint if isinstance(hint, Mapping) else {"content": str(hint)})
            return manager.detail(workspace_id) | {"started": started.to_dict()}

    def rename(self, workspace_id: str, title: str) -> dict[str, Any]:
        if not str(title).strip():
            raise ValueError("workspace_title_required")
        self._materialize(workspace_id)
        with self.store.transaction(immediate=True) as connection:
            connection.execute("UPDATE workspace_meta SET title=?, updated_at=? WHERE workspace_id=?", (str(title).strip(), _now(), workspace_id))
        return self.detail(workspace_id)

    def set_status(self, workspace_id: str, status: str) -> dict[str, Any]:
        status = str(status).strip()
        if status not in {"active", "stopped", "completed"}:
            raise ValueError("workspace_status_invalid")
        self._materialize(workspace_id)
        run_ids = self._run_ids(workspace_id)
        if status == "completed" and any(self._run_summary(run_id)["status"] not in TERMINAL for run_id in run_ids):
            raise ValueError("workspace_has_open_runs")
        for run_id in run_ids:
            current = self._run_summary(run_id)["status"]
            if status == "stopped" and current not in TERMINAL and current != "paused_budget":
                self.service.pause(run_id, reason="workspace_stopped")
            elif status == "active" and current == "paused_budget":
                self.service.resume(run_id, execute=False)
        with self.store.transaction(immediate=True) as connection:
            if status in {"stopped", "completed"}:
                connection.execute(
                    "UPDATE workspace_meta SET reason_worker='',reason_trigger='',reason_started_at='',"
                    "reason_last_heartbeat_at='' WHERE workspace_id=?", (workspace_id,)
                )
                connection.execute(
                    "UPDATE workspace_intents SET worker='',last_heartbeat_at='' "
                    "WHERE workspace_id=? AND concluded_at=''", (workspace_id,)
                )
            connection.execute("UPDATE workspace_meta SET status=?, updated_at=? WHERE workspace_id=?", (status, _now(), workspace_id))
        return self.detail(workspace_id)

    def add_hint(self, workspace_id: str, payload: Mapping[str, Any]) -> dict[str, Any]:
        self._materialize(workspace_id)
        content = str(payload.get("content") or "").strip()
        creator = str(payload.get("creator") or "operator").strip()
        if not content:
            raise ValueError("workspace_hint_required")
        hint_id = "hint-" + uuid4().hex[:20]
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO workspace_hints(hint_id,workspace_id,content,creator,created_at) VALUES(?,?,?,?,?)",
                (hint_id, workspace_id, content, creator, _now()),
            )
        run_ids = self._run_ids(workspace_id)
        if run_ids:
            latest = self._run_summary(run_ids[0])
            if latest["status"] not in TERMINAL:
                self.service.submit_message(run_ids[0], content, message_id=hint_id)
        with self.store.transaction(immediate=True) as connection:
            connection.execute("UPDATE workspace_meta SET updated_at=? WHERE workspace_id=?", (_now(), workspace_id))
        return self.detail(workspace_id)

    def add_intent(self, workspace_id: str, payload: Mapping[str, Any]) -> dict[str, Any]:
        description = str(payload.get("description") or "").strip()
        if not description:
            raise ValueError("workspace_intent_required")
        with self._creation_transaction() as manager:
            manager._materialize(workspace_id)
            run_ids = manager._run_ids(workspace_id)
            targets = list(payload.get("targets") or dict.fromkeys(
                target for run_id in reversed(run_ids) for target in manager._run_summary(run_id)["targets"]))
            started = manager.service.start({
                "session_id": workspace_id, "objective": description,
                "targets": targets,
                "max_actions": int(payload.get("max_actions", 0)),
                "starting_context": {"workspace_id": workspace_id, "intent_source": "operator"},
            })
            for view in started.runs:
                manager._ensure_intent(workspace_id, view.run.run_id, description)
            with manager.store.transaction() as connection:
                connection.execute("UPDATE workspace_meta SET status='active', updated_at=? WHERE workspace_id=?", (_now(), workspace_id))
            return {"intent": {"run_id": started.run_ids[0], "run_ids": list(started.run_ids), "description": description},
                    "started": started.to_dict(), **manager.detail(workspace_id)}

    def delete(self, workspace_id: str) -> None:
        self._require(workspace_id)
        for run_id in self._run_ids(workspace_id):
            self.service.delete_conversation(run_id)
        with self.store.transaction(immediate=True) as connection:
            connection.execute("DELETE FROM workspace_meta WHERE workspace_id=?", (workspace_id,))

    def _ensure_intent(self, workspace_id: str, run_id: str, description: str) -> str:
        with self.store.transaction(immediate=True) as connection:
            row = connection.execute("SELECT intent_id FROM workspace_intents WHERE run_id=?", (run_id,)).fetchone()
            if row is not None:
                return str(row["intent_id"])
            intent_id = "intent-" + uuid4().hex[:20]
            now = _now()
            connection.execute(
                "INSERT INTO workspace_intents(intent_id,workspace_id,run_id,description,creator,created_at) VALUES(?,?,?,?,?,?)",
                (intent_id, workspace_id, run_id, description, "operator", now),
            )
            return intent_id

    def _materialize(self, workspace_id: str) -> None:
        """Persist metadata only when a legacy run workspace is edited."""
        self._require(workspace_id)
        if self._meta(workspace_id) is not None:
            return
        summary = self._summary(workspace_id)
        with self.store.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT OR IGNORE INTO workspace_meta "
                "(workspace_id,title,status,origin,goal,created_at,updated_at) VALUES(?,?,?,?,?,?,?)",
                tuple(summary[key] for key in ("workspace_id", "title", "status", "origin", "goal", "created_at", "updated_at")),
            )

    def _expire_leases(self, workspace_id: str, connection=None) -> None:
        if connection is None:
            with self.store.transaction(immediate=True) as connection:
                self._expire_leases(workspace_id, connection)
            return
        now = _now()
        connection.execute(
            "UPDATE workspace_intents SET worker='',last_heartbeat_at='' "
            "WHERE workspace_id=? AND concluded_at='' AND worker!='' "
            "AND COALESCE(julianday(last_heartbeat_at),0) <= julianday(?, '-15 minutes')", (workspace_id, now)
        )
        connection.execute(
            "UPDATE workspace_meta SET reason_worker='',reason_trigger='',reason_started_at='',reason_last_heartbeat_at='' "
            "WHERE workspace_id=? AND reason_worker!='' "
            "AND COALESCE(julianday(reason_last_heartbeat_at),0) <= julianday(?, '-15 minutes')", (workspace_id, now)
        )

    def _intent_action(self, workspace_id: str, intent_id: str, worker: str,
                       action: str, description: str = "") -> dict[str, Any]:
        self._materialize(workspace_id)
        worker = str(worker or "").strip()
        if not worker:
            raise ValueError("workspace_worker_required")
        with self.store.transaction(immediate=True) as connection:
            self._expire_leases(workspace_id, connection)
            # Legacy projections become real intents on their first claim.
            if intent_id.startswith("run:"):
                run_id = intent_id[4:]
                run = connection.execute(
                    f"SELECT state_json FROM operations WHERE {_WORKSPACE_ID}=? AND run_id=?", (workspace_id, run_id)
                ).fetchone()
                if run is None:
                    raise KeyError(f"workspace_intent_not_found:{intent_id}")
                existing = connection.execute("SELECT intent_id FROM workspace_intents WHERE run_id=?", (run_id,)).fetchone()
                if existing is not None:
                    intent_id = existing["intent_id"]
                elif action == "claim":
                    state = json.loads(run["state_json"])
                    connection.execute(
                        "INSERT INTO workspace_intents(intent_id,workspace_id,run_id,description,creator,created_at) VALUES(?,?,?,?,?,?)",
                        (intent_id, workspace_id, run_id, state["goal"]["objective"], "operator", state["created_at"]),
                    )
            row = connection.execute(
                "SELECT * FROM workspace_intents WHERE workspace_id=? AND intent_id=?", (workspace_id, intent_id)
            ).fetchone()
            if row is None:
                raise KeyError(f"workspace_intent_not_found:{intent_id}")
            if row["concluded_at"]:
                raise ValueError("workspace_intent_concluded")
            if action == "claim":
                if row["worker"] and row["worker"] != worker:
                    raise ValueError(f"workspace_intent_claimed:{row['worker']}")
            elif row["worker"] != worker:
                raise ValueError("workspace_intent_not_owned")
            now = _now()
            if action == "conclude":
                description = str(description or "").strip()
                if not description:
                    raise ValueError("workspace_fact_required")
                run = json.loads(connection.execute(
                    "SELECT state_json FROM operations WHERE run_id=?", (row["run_id"],)
                ).fetchone()[0])
                if run["status"] not in TERMINAL:
                    raise ValueError("workspace_intent_run_still_open")
                fact = FactRecord(
                    fact_id="fact-" + uuid4().hex[:20], run_id=row["run_id"],
                    branch_id=run["branch_id"], plan_revision=run["plan_revision"],
                    key="workspace_intent:" + intent_id, version=1, created_at=now,
                    value={"description": description, "source": "operator_conclusion", "creator": worker,
                           "intent_id": intent_id, "verified": False},
                )
                # Fact and intent edge commit together; no evidence node is fabricated.
                connection.execute(
                    "INSERT INTO facts(fact_id,run_id,branch_id,fact_key,version,valid,fact_json,created_at) VALUES(?,?,?,?,?,?,?,?)",
                    (fact.fact_id, fact.run_id, fact.branch_id, fact.key, fact.version, 1,
                     json.dumps(fact.to_dict(), ensure_ascii=False), now),
                )
                connection.execute(
                    "UPDATE workspace_intents SET to_fact=?,concluded_at=?,last_heartbeat_at=? WHERE intent_id=?",
                    (fact.fact_id, now, now, intent_id),
                )
            else:
                connection.execute(
                    "UPDATE workspace_intents SET worker=?,last_heartbeat_at=? WHERE intent_id=?",
                    ("" if action == "release" else worker, "" if action == "release" else now, intent_id),
                )
            connection.execute("UPDATE workspace_meta SET updated_at=? WHERE workspace_id=?", (now, workspace_id))
        return self.detail(workspace_id)

    def claim_intent(self, workspace_id: str, intent_id: str, worker: str) -> dict[str, Any]:
        return self._intent_action(workspace_id, intent_id, worker, "claim")

    def heartbeat_intent(self, workspace_id: str, intent_id: str, worker: str) -> dict[str, Any]:
        return self._intent_action(workspace_id, intent_id, worker, "heartbeat")

    def release_intent(self, workspace_id: str, intent_id: str, worker: str) -> dict[str, Any]:
        return self._intent_action(workspace_id, intent_id, worker, "release")

    def conclude_intent(self, workspace_id: str, intent_id: str, worker: str, description: str) -> dict[str, Any]:
        return self._intent_action(workspace_id, intent_id, worker, "conclude", description)

    def _reason_action(self, workspace_id: str, worker: str, action: str, trigger: str = "") -> dict[str, Any]:
        self._materialize(workspace_id)
        worker, trigger = str(worker or "").strip(), str(trigger or "").strip()
        if not worker or (action == "claim" and not trigger):
            raise ValueError("workspace_reason_required")
        with self.store.transaction(immediate=True) as connection:
            self._expire_leases(workspace_id, connection)
            meta = connection.execute("SELECT * FROM workspace_meta WHERE workspace_id=?", (workspace_id,)).fetchone()
            if meta is None:
                raise KeyError(f"workspace_not_found:{workspace_id}")
            if action == "claim":
                if meta["status"] != "active":
                    raise ValueError("workspace_not_active")
                if meta["reason_worker"] and meta["reason_worker"] != worker:
                    raise ValueError(f"workspace_reason_claimed:{meta['reason_worker']}")
            elif meta["reason_worker"] != worker:
                raise ValueError("workspace_reason_not_owned")
            now = _now()
            if action == "release":
                values = ("", "", "", "")
            else:
                values = (worker, trigger if action == "claim" else meta["reason_trigger"], meta["reason_started_at"] or now, now)
            connection.execute(
                "UPDATE workspace_meta SET reason_worker=?,reason_trigger=?,reason_started_at=?,"
                "reason_last_heartbeat_at=?,updated_at=? WHERE workspace_id=?", (*values, now, workspace_id),
            )
        return self.detail(workspace_id)

    def claim_reason(self, workspace_id: str, worker: str, trigger: str) -> dict[str, Any]:
        return self._reason_action(workspace_id, worker, "claim", trigger)

    def heartbeat_reason(self, workspace_id: str, worker: str) -> dict[str, Any]:
        return self._reason_action(workspace_id, worker, "heartbeat")

    def release_reason(self, workspace_id: str, worker: str) -> dict[str, Any]:
        return self._reason_action(workspace_id, worker, "release")

    def export(self, workspace_id: str) -> dict[str, Any]:
        detail = self.detail(workspace_id)
        sessions = []
        for run in detail["runs"]:
            sessions.append({"run": run, "session": self.service.export_session(run["run_id"])})
        return {"schema_version": 1, "workspace": detail["workspace"], "facts": detail["facts"],
                "intents": detail["intents"], "hints": detail["hints"], "runs": sessions}

    def model_context(self, workspace_id: str) -> dict[str, Any]:
        """Read the shared board on each turn, including hints added after run creation."""
        with self.store.connection() as connection:
            # ContextSelector passes the Runtime session ID, including batch children.
            owner = connection.execute(
                f"SELECT {_WORKSPACE_ID} FROM operations WHERE session_id=? LIMIT 1", (workspace_id,)
            ).fetchone()
            if owner is not None:
                workspace_id = str(owner[0])
            meta = connection.execute(
                "SELECT title,goal,origin FROM workspace_meta WHERE workspace_id=?", (workspace_id,)
            ).fetchone()
            if meta is None:
                return {}
            hints = [dict(row) for row in connection.execute(
                "SELECT hint_id,content,creator,created_at FROM workspace_hints WHERE workspace_id=? "
                "ORDER BY created_at,hint_id", (workspace_id,))]
            intents = [dict(row) for row in connection.execute(
                "SELECT intent_id,description,to_fact,concluded_at FROM workspace_intents WHERE workspace_id=? "
                "ORDER BY created_at,intent_id", (workspace_id,))]
            facts = [json.loads(row[0]) for row in connection.execute(
                "SELECT fact_json FROM facts WHERE valid=1 AND run_id IN "
                f"(SELECT run_id FROM operations WHERE {_WORKSPACE_ID}=?) ORDER BY created_at,fact_id", (workspace_id,))]
        return {"workspace_id": workspace_id, **dict(meta), "hints": hints,
                "intents": intents, "facts": facts}

    def _require(self, workspace_id: str) -> None:
        if not self._run_ids(workspace_id) and self._meta(workspace_id) is None:
            raise KeyError(f"workspace_not_found:{workspace_id}")


__all__ = ["WorkspaceManager"]
