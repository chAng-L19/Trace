from __future__ import annotations

import time
import threading
import math
import random
from dataclasses import replace
from datetime import datetime, timezone
from http.client import IncompleteRead
from typing import Any
from ..runtime.model_common import _utc_datetime
from .model_continuation import prepare_continuation, hydrate_continuation, persist_continuation


def is_context_overflow(error: BaseException) -> bool:
    """Recognize provider window errors without trusting provider payloads."""
    values = [str(error)]
    for name in ("code", "type", "error_type", "status"):
        value = getattr(error, name, None)
        if value is not None:
            values.append(str(value))
    marker = " ".join(values).lower().replace("-", "_").replace(" ", "_")
    return any(
        token in marker
        for token in (
            "context_length_exceeded",
            "context_window_exceeded",
            "prompt_is_too_long",
            "maximum_context_length",
            "too_many_tokens",
        )
    )


def classify_provider_error(error: BaseException) -> tuple[str, bool]:
    """Conservative retry allowlist; provider text alone cannot override a 4xx."""
    code = str(getattr(error, "code", "")).lower().replace("-", "_")
    marker = (code + " " + type(error).__name__ + " " + str(error)).lower()
    raw_status = getattr(error, "status", getattr(error, "status_code", 0))
    try:
        status = int(raw_status)
    except (ValueError, TypeError):
        status = 0
    if (status in {401, 403} or code in {"invalid_api_key", "authentication_error", "permission_denied"}
            or "missing_credentials" in marker):
        return "auth", False
    if any(value in marker for value in ("insufficient_quota", "quota_exceeded", "billing_hard_limit", "billing_not_active")):
        return "quota", False
    if code in {"refusal", "refused"} or any(value in marker for value in ("content_filter", "content_policy_violation", "safety_refusal")):
        return "refusal", False
    if status in {0, 400, 413} and is_context_overflow(error):
        return "context", False
    if status == 429:
        return "rate_limit", True
    if 400 <= status < 500:
        return "protocol", False
    if 500 <= status < 600:
        return "transport", True
    if isinstance(error, (ConnectionError, TimeoutError, IncompleteRead)):
        return "transport", True
    if not status and code in {"rate_limit_exceeded", "rate_limit_error"}:
        return "rate_limit", True
    return "protocol", False


def check_turn_budget(loop: Any, run_id: str) -> Any:
    if loop._is_cancelled(run_id) or loop._is_interrupted(run_id):
        raise loop._interrupted_error("model_loop_interrupted")
    current = loop.service._enforce_runtime_budget(run_id)
    if current.run.status == "paused_budget":
        raise loop._interrupted_error("model_loop_budget_paused")
    if current.terminal.terminal:
        raise loop._interrupted_error("model_loop_terminal")
    return current


def deadline_request(loop: Any, request: Any) -> Any:
    current = check_turn_budget(loop, request.run_id)
    deadline = _utc_datetime(current.run.budget.deadline)
    if deadline is None:
        return request
    remaining = (deadline - datetime.now(timezone.utc)).total_seconds()
    if remaining <= 0:
        check_turn_budget(loop, request.run_id)
        raise loop._interrupted_error("model_loop_budget_paused")
    return replace(request, metadata={**request.metadata, "runtime_deadline": deadline.isoformat(),
                                      "remaining_time_seconds": remaining})


def invoke_with_deadline(loop: Any, request: Any) -> Any:
    """Cancel this request at the run deadline; transport also enforces the bound."""
    deadline = _utc_datetime(request.metadata.get("runtime_deadline"))
    if deadline is None:
        return loop._invoke(request)
    remaining = (deadline - datetime.now(timezone.utc)).total_seconds()
    if remaining <= 0:
        check_turn_budget(loop, request.run_id)
        raise loop._interrupted_error("model_loop_budget_paused")
    finished = threading.Event()

    def expire() -> None:
        if not finished.is_set():
            try:
                loop.model.cancel(request.request_id)
            except Exception as error:
                loop.service.runtime.store.append_event(request.run_id, "model_deadline_cancel_failed", {
                    "request_id": request.request_id, "error_type": type(error).__name__,
                })

    timer = threading.Timer(remaining, expire)
    timer.daemon = True
    timer.start()
    try:
        return loop._invoke(request)
    finally:
        finished.set()
        timer.cancel()
        timer.join(timeout=0.2)


def retry_backoff(loop: Any, run_id: str, attempt: int, *, retry_after: float = 0.0) -> None:
    base = min(2.0, 0.25 * 2 ** min(attempt - 1, 3))
    server_delay = (min(300.0, retry_after) if isinstance(retry_after, (int, float))
                    and not isinstance(retry_after, bool) and math.isfinite(retry_after) else 0.0)
    deadline = time.monotonic() + max(0.0, server_delay) + random.uniform(base / 2, base)
    while True:
        check_turn_budget(loop, run_id)
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return
        time.sleep(min(0.05, remaining))


def run_model_turn(loop: Any, view: Any) -> tuple[Any, Any]:
    last_error: BaseException | None = None
    attempt = 0
    overflow_retry = 0
    force_compaction = False
    while True:
        check_turn_budget(loop, view.run.run_id)
        if not getattr(loop.model, "ready", True):
            loop.service.runtime.pause_run(view.run.run_id, reason="missing_credentials")
            raise loop._interrupted_error("missing_credentials")
        request = loop._request(
            view,
            attempt=attempt,
            force_compaction=force_compaction,
            overflow_retry=overflow_retry,
        )
        force_compaction = False
        request = prepare_continuation(loop, request)
        request = deadline_request(loop, request)
        loop._save_request(request)
        loop._track(loop._active_requests, view.run.run_id, request.request_id, add=True)
        try:
            # Close/pause may race context construction and request registration.
            if loop._is_interrupted(view.run.run_id) or loop._is_cancelled(view.run.run_id):
                raise loop._interrupted_error("model_loop_interrupted")
            response = invoke_with_deadline(loop, hydrate_continuation(loop, request))
            response = persist_continuation(loop, request, response)
            validated = loop._validate_response(request, response)
            if loop.streaming:
                loop.service.runtime.store.append_event(request.run_id, "model_stream_status", {
                    "request_id": request.request_id, "status": "completed", "provisional": False,
                })
            return validated, request
        except loop._integrity_error as exc:
            loop._save_failure_response(request, exc)
            if loop.streaming:
                loop.service.runtime.store.append_event(request.run_id, "model_stream_status", {
                    "request_id": request.request_id, "status": "integrity_error", "provisional": False,
                })
            raise
        except BaseException as exc:
            last_error = exc
            process_signal = isinstance(exc, (KeyboardInterrupt, SystemExit))
            partial = getattr(threading.current_thread(), "model_partial_response", None)
            partial_metadata = partial[1] if isinstance(partial, tuple) and partial[0] == request.request_id else {}
            refused = (partial_metadata.get("refusal") or partial_metadata.get("refusal_text")
                       or partial_metadata.get("response_category") == "refusal")
            loop._save_failure_response(request, loop._interrupted_error("process_shutdown") if process_signal else exc)
            if loop.streaming:
                loop.service.runtime.store.append_event(request.run_id, "model_stream_status", {
                    "request_id": request.request_id, "status": "interrupted", "provisional": False,
                })
            if process_signal:
                raise
            if (isinstance(exc, loop._interrupted_error)
                    or loop._is_interrupted(view.run.run_id) or loop._is_cancelled(view.run.run_id)):
                raise loop._interrupted_error("model_loop_interrupted") from exc
            check_turn_budget(loop, view.run.run_id)
            category, retryable = ("refusal", False) if refused else classify_provider_error(exc)
            compact = category == "context" and not overflow_retry
            server_delay = getattr(exc, "retry_after", 0.0)
            if (isinstance(server_delay, bool) or not isinstance(server_delay, (int, float))
                    or not math.isfinite(server_delay) or server_delay < 0):
                server_delay = 0.0
            retry = retryable and attempt < loop.max_retries and server_delay <= 300.0
            loop.service.runtime.store.append_event(request.run_id, "model_provider_recovery", {
                "request_id": request.request_id, "category": category,
                "retryable": retryable, "attempt": attempt, "retry_limit": loop.max_retries,
                "action": "compact" if compact else "retry" if retry else "stop",
                "retry_after_seconds": server_delay,
                "retry_delay_limit_exceeded": server_delay > 300.0,
            })
            if compact:
                overflow_retry = 1
                force_compaction = True
                continue
            if retry:
                attempt += 1
                retry_backoff(loop, view.run.run_id, attempt,
                              retry_after=server_delay)
                continue
            break
        finally:
            loop._track(loop._active_requests, view.run.run_id, request.request_id, add=False)
    raise loop._loop_error(f"model_provider_retries_exhausted:{last_error}") from last_error
