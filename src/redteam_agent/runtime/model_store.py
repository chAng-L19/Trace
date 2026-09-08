from __future__ import annotations

import sqlite3
from typing import Any, Mapping

from .model_records import (
    ModelObservationRecord,
    ModelRequestRecord,
    ModelResponseRecord,
)
from ..core import ModelStreamEvent
from .store_common import ImmutableRecordError, _dump, _load


class ModelStoreMixin:
    def save_model_request(self, record: ModelRequestRecord) -> None:
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            self._immutable_insert(
                connection,
                table="model_requests",
                key_column="request_id",
                key=record.request_id,
                json_column="request_json",
                serialized=serialized,
                sql=(
                    "INSERT INTO model_requests(request_id, run_id, prompt_hash, provider, model, "
                    "capabilities_json, request_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)"
                ),
                values=(
                    record.request_id,
                    record.run_id,
                    record.prompt_hash,
                    record.provider,
                    record.model,
                    _dump(dict(record.capabilities)),
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="model_request",
                raw_table="model_requests",
                raw_id=record.request_id,
                raw_json=serialized,
                created_at=record.created_at,
            )

    def save_model_response(self, record: ModelResponseRecord) -> None:
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            request = connection.execute(
                "SELECT run_id FROM model_requests WHERE request_id=?",
                (record.request_id,),
            ).fetchone()
            if request is None or str(request["run_id"]) != record.run_id:
                raise ImmutableRecordError(f"model_response_request_mismatch:{record.request_id}")
            self._immutable_insert(
                connection,
                table="model_responses",
                key_column="request_id",
                key=record.request_id,
                json_column="response_json",
                serialized=serialized,
                sql=(
                    "INSERT INTO model_responses(request_id, run_id, status, provider, model, "
                    "response_hash, claimed_response_hash, usage_json, response_json, created_at) "
                    "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
                ),
                values=(
                    record.request_id,
                    record.run_id,
                    record.status,
                    record.provider,
                    record.model,
                    record.response_hash,
                    record.claimed_response_hash,
                    _dump(dict(record.usage)),
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="model_response",
                raw_table="model_responses",
                raw_id=record.request_id,
                raw_json=serialized,
                created_at=record.created_at,
            )

    def save_model_stream_event(self, event: ModelStreamEvent) -> None:
        serialized = _dump(event.to_dict())
        with self.transaction(immediate=True) as connection:
            try:
                connection.execute(
                    "INSERT INTO model_stream_events(request_id, sequence, event_type, event_json) "
                    "VALUES(?, ?, ?, ?)",
                    (event.request_id, event.sequence, event.event_type, serialized),
                )
            except sqlite3.IntegrityError as exc:
                row = connection.execute(
                    "SELECT event_json FROM model_stream_events WHERE request_id=? AND sequence=?",
                    (event.request_id, event.sequence),
                ).fetchone()
                if row is None or str(row["event_json"]) != serialized:
                    raise ImmutableRecordError(
                        f"immutable_model_stream_event:{event.request_id}:{event.sequence}"
                    ) from exc

    def save_model_observation(self, record: ModelObservationRecord) -> None:
        serialized = _dump(record.to_dict())
        with self.transaction(immediate=True) as connection:
            request = connection.execute(
                "SELECT run_id FROM model_requests WHERE request_id=?",
                (record.request_id,),
            ).fetchone()
            if request is None or str(request["run_id"]) != record.run_id:
                raise ImmutableRecordError(f"model_observation_request_mismatch:{record.request_id}")
            self._immutable_insert(
                connection,
                table="model_observations",
                key_column="observation_id",
                key=record.observation_id,
                json_column="observation_json",
                serialized=serialized,
                sql=(
                    "INSERT INTO model_observations(observation_id, request_id, run_id, action_id, "
                    "call_id, tool_name, status, input_hash, output_hash, observation_json, created_at) "
                    "VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
                ),
                values=(
                    record.observation_id,
                    record.request_id,
                    record.run_id,
                    record.action_id,
                    record.call_id,
                    record.tool_name,
                    record.status,
                    record.input_hash,
                    record.output_hash,
                    serialized,
                    record.created_at,
                ),
            )
            self._insert_journal_entry(
                connection,
                run_id=record.run_id,
                entry_type="model_observation",
                raw_table="model_observations",
                raw_id=record.observation_id,
                raw_json=serialized,
                created_at=record.created_at,
            )

    def model_requests(self, run_id: str) -> tuple[ModelRequestRecord, ...]:
        return self._model_records(
            "SELECT request_json FROM model_requests WHERE run_id=? ORDER BY rowid",
            run_id,
            ModelRequestRecord,
        )

    def model_responses(self, run_id: str) -> tuple[ModelResponseRecord, ...]:
        return self._model_records(
            "SELECT response_json FROM model_responses WHERE run_id=? ORDER BY rowid",
            run_id,
            ModelResponseRecord,
        )

    def model_observations(self, run_id: str) -> tuple[ModelObservationRecord, ...]:
        return self._model_records(
            "SELECT observation_json FROM model_observations WHERE run_id=? "
            "ORDER BY rowid",
            run_id,
            ModelObservationRecord,
        )

    def model_stream_events(self, request_id: str) -> tuple[ModelStreamEvent, ...]:
        with self.connection() as connection:
            rows = connection.execute(
                "SELECT event_json FROM model_stream_events WHERE request_id=? ORDER BY sequence",
                (request_id,),
            ).fetchall()
        return tuple(
            ModelStreamEvent.from_dict(payload)
            for row in rows
            if isinstance((payload := _load(row["event_json"], None)), Mapping)
        )

    def _model_records(self, query: str, key: str, record_type: Any) -> tuple[Any, ...]:
        with self.connection() as connection:
            rows = connection.execute(query, (key,)).fetchall()
        records = []
        for row in rows:
            payload = _load(row[0], None)
            if isinstance(payload, Mapping):
                records.append(record_type.from_dict(payload))
        return tuple(records)
