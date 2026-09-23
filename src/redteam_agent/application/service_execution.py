from __future__ import annotations

import threading
import time
from contextlib import contextmanager
from functools import wraps
from typing import Any, Mapping
from uuid import uuid4

from .contracts import AgentRunView, BudgetDelta
from .model_loop import AgentLoop, ModelInterruptedError


def service_write(operation):
    """Fence new writes during close and let admitted nested writes finish."""
    @wraps(operation)
    def guarded(self, *args, **kwargs):
        with self._write_scope():
            return operation(self, *args, **kwargs)
    return guarded


class ServiceExecutionMixin:
    """Bounded execution and process shutdown for the canonical service."""

    @property
    def closing(self) -> bool:
        return self._closing

    def _ensure_open(self) -> None:
        if self._closing:
            raise RuntimeError("agent_service_closing")

    @contextmanager
    def _write_scope(self):
        with self._model_condition:
            depth = getattr(self._write_local, "depth", 0)
            if not depth:
                self._ensure_open()
                self._active_writes += 1
            self._write_local.depth = depth + 1
        try:
            yield
        finally:
            with self._model_condition:
                self._write_local.depth -= 1
                if not depth:
                    self._active_writes -= 1
                    self._model_condition.notify_all()

    def close(self, *, timeout_seconds: float = 10.0) -> None:
        """Interrupt before closing transports; an uncooperative provider stays unknown."""
        timeout_seconds = max(0.0, float(timeout_seconds))
        with self._model_condition:
            if not self._closing:
                self._closing = True
                self._shutdown_deadline = time.monotonic() + timeout_seconds
                self._shutdown_error = ""
                self._shutdown_errors = []
                run_ids = tuple(self._active_runs)
                # Persist the pause before cancellation can race a successful response.
                for run_id in run_ids:
                    try:
                        self.runtime.pause_run(run_id, reason="service_shutdown")
                    except (ValueError, KeyError):
                        pass
                    except Exception as exc:
                        self._shutdown_errors.append(exc)
                    try:
                        self.runtime.store.append_event(run_id, "service_shutdown_interrupted", {
                            "status": "interruption_requested", "reason": "service_shutdown",
                        })
                    except Exception as exc:
                        self._shutdown_errors.append(exc)
                threading.Thread(target=self._close_resources, args=(run_ids,),
                                 name="agent-service-shutdown", daemon=True).start()
            remaining = max(0.0, self._shutdown_deadline - time.monotonic())
        if not self._close_complete.wait(remaining):
            with self._model_condition:
                unresolved = tuple(self._active_runs)
                first_timeout = not getattr(self, "_shutdown_timeout_recorded", False)
                self._shutdown_timeout_recorded = True
            if first_timeout:
                for run_id in unresolved:
                    self.runtime.store.append_event(run_id, "service_shutdown_unknown", {
                        "status": "unknown", "reason": "shutdown_timeout", "resources_closed": False,
                    })
            raise TimeoutError("agent_service_shutdown_timeout")
        elif self._shutdown_error:
            raise RuntimeError(f"agent_service_cleanup_failed:{self._shutdown_error}") from None

    def _close_resources(self, run_ids: tuple[str, ...]) -> None:
        errors: list[Exception] = self._shutdown_errors

        def attempt(operation, *args, **kwargs) -> None:
            try:
                operation(*args, **kwargs)
            except Exception as exc:
                errors.append(exc)

        try:
            for run_id in run_ids:
                attempt(self._interrupt_model_loops, run_id)
                attempt(self._cancel_run_workers, run_id, include_waiting=False)
            with self._model_condition:
                self._model_condition.wait_for(lambda: not self._active_runs and not self._active_writes,
                    timeout=max(0.0, self._shutdown_deadline - time.monotonic()))
                unresolved = set(self._active_runs)
            for run_id in run_ids:
                attempt(self.runtime.store.append_event, run_id, "service_shutdown_settled", {
                    "status": "unknown" if run_id in unresolved else "interrupted",
                    "reason": "shutdown_timeout" if run_id in unresolved else "service_shutdown",
                })
        except Exception as exc:
            errors.append(exc)
        finally:
            close = getattr(self.workers, "close", None)
            if callable(close):
                attempt(close)
            attempt(self.runtime.broker.close)
            if errors:
                error_types = sorted({type(error).__name__ for error in errors})
                for run_id in run_ids:
                    attempt(self.runtime.store.append_event, run_id, "service_shutdown_cleanup_failed", {
                        "status": "unknown", "error_types": error_types,
                    })
                self._shutdown_error = ",".join(sorted({type(error).__name__ for error in errors}))
            self._close_complete.set()

    def run(
        self,
        run_id: str,
        budget_delta: BudgetDelta | Mapping[str, Any] | None = None,
        *,
        max_actions: int | None = None,
        run_until_pause: bool = True,
        max_cycles: int = 32,
    ) -> AgentRunView:
        with self._model_condition:
            self._ensure_open()
            self._active_runs[run_id] = self._active_runs.get(run_id, 0) + 1
        try:
            return self._run_registered(run_id, budget_delta, max_actions=max_actions,
                                        run_until_pause=run_until_pause, max_cycles=max_cycles)
        except (KeyboardInterrupt, SystemExit):
            try:
                self.runtime.pause_run(run_id, reason="service_shutdown")
            except (ValueError, KeyError):
                pass
            self._interrupt_model_loops(run_id)
            self.runtime.store.append_event(run_id, "service_shutdown_interrupted", {
                "status": "interrupted", "reason": "process_signal",
            })
            raise
        finally:
            with self._model_condition:
                self._active_runs[run_id] -= 1
                if not self._active_runs[run_id]:
                    del self._active_runs[run_id]
                self._model_condition.notify_all()

    def _run_registered(self, run_id, budget_delta, *, max_actions, run_until_pause, max_cycles):
        before = self.status(run_id)
        delta = BudgetDelta.from_value(budget_delta)
        if delta.changes_budget and before.run.status in {"completed", "failed", "cancelled"}:
            raise ValueError(f"operation_terminal:{before.run.status}")
        if delta.changes_budget:
            arguments = {
                "actions": delta.actions,
                "tokens": delta.tokens,
                "time_seconds": delta.time_seconds,
                "deadline": delta.deadline,
                "acknowledge_missing_usage": delta.acknowledge_missing_usage,
            }
            if delta.idempotency_key:
                self.runtime.apply_budget_delta_once(
                    run_id,
                    idempotency_key=delta.idempotency_key,
                    **arguments,
                )
            else:
                self.runtime.apply_budget_delta(run_id, **arguments)
        with self._model_lock:
            loop = self.agent_loop
            if loop is not None:
                active = self._active_model_loops.setdefault(run_id, {})
                active[loop] = active.get(loop, 0) + 1
        if loop is None:
            return self._resume_runtime(run_id, max_actions=max_actions)
        try:
            return self._run_model_loop(loop, run_id, max_actions=max_actions,
                                        run_until_pause=run_until_pause, max_cycles=max_cycles)
        finally:
            with self._model_condition:
                active = self._active_model_loops[run_id]
                active[loop] -= 1
                if not active[loop]:
                    del active[loop]
                if not active:
                    del self._active_model_loops[run_id]
                self._model_condition.notify_all()

    def _run_model_loop(
        self,
        loop: AgentLoop,
        run_id: str,
        *,
        max_actions: int | None,
        run_until_pause: bool,
        max_cycles: int,
    ) -> AgentRunView:
        ttl = 30.0
        token = self.runtime.store.acquire_lease(
            run_id,
            "__model_loop__",
            f"agent-service:model-loop:{uuid4().hex}",
            ttl_seconds=ttl,
        )
        if token is None:
            return self.status(run_id)
        stopped = threading.Event()

        def renew() -> None:
            while not stopped.wait(ttl / 3):
                try:
                    if self.runtime.store.renew_lease(token, ttl_seconds=ttl) is not None:
                        continue
                except Exception:
                    pass
                loop.interrupt(run_id)
                return

        heartbeat = threading.Thread(target=renew, name="model-loop-lease", daemon=True)
        try:
            heartbeat.start()
            try:
                result = loop.run(run_id, max_actions=max_actions, run_until_pause=run_until_pause, max_cycles=max_cycles)
            except ModelInterruptedError:
                result = self.status(run_id)
                if result.run.status not in {"paused_budget", "cancelling", "cancelled"}:
                    raise
        finally:
            stopped.set()
            heartbeat.join(timeout=5)
            self.runtime.store.release_lease(token)
        state = self.runtime.store.load_operation(run_id)
        if state is not None and state.status == "cancelling":
            return self._view(
                self.runtime.cancel(run_id, reason=state.cancel_reason or "cancel_requested")
            )
        return result
