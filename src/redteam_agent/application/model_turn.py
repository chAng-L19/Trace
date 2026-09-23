from __future__ import annotations

from typing import Any
from .model_continuation import prepare_continuation, hydrate_continuation, persist_continuation


def is_context_overflow(error: BaseException) -> bool:
    """Recognize provider window errors without trusting provider payloads."""
    values = [str(error)]
    for name in ("code", "type", "error_type", "status"):
        value = getattr(error, name, None)
        if value is not None:
            values.append(str(value))
    marker = " ".join(values).lower().replace("-", "_")
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


def run_model_turn(loop: Any, view: Any) -> tuple[Any, Any]:
    last_error: BaseException | None = None
    attempt = 0
    overflow_retry = 0
    while True:
        if loop._is_cancelled(view.run.run_id):
            raise loop._interrupted_error("model_loop_cancelled")
        if loop._is_interrupted(view.run.run_id):
            raise loop._interrupted_error("model_loop_interrupted")
        current = loop.service._enforce_runtime_budget(view.run.run_id)
        if current.run.status == "paused_budget":
            raise loop._interrupted_error("model_loop_budget_paused")
        if current.terminal.terminal:
            raise loop._interrupted_error("model_loop_terminal")
        if not getattr(loop.model, "ready", True):
            loop.service.runtime.pause_run(view.run.run_id, reason="missing_credentials")
            raise loop._interrupted_error("missing_credentials")
        request = loop._request(
            view,
            attempt=attempt,
            force_compaction=bool(overflow_retry),
            overflow_retry=overflow_retry,
        )
        request = prepare_continuation(loop, request)
        loop._save_request(request)
        loop._track(loop._active_requests, view.run.run_id, request.request_id, add=True)
        try:
            # Close/pause may race context construction and request registration.
            if loop._is_interrupted(view.run.run_id) or loop._is_cancelled(view.run.run_id):
                raise loop._interrupted_error("model_loop_interrupted")
            response = loop._invoke(hydrate_continuation(loop, request))
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
            loop._save_failure_response(request, loop._interrupted_error("process_shutdown") if process_signal else exc)
            if loop.streaming:
                loop.service.runtime.store.append_event(request.run_id, "model_stream_status", {
                    "request_id": request.request_id, "status": "interrupted", "provisional": False,
                })
            if process_signal:
                raise
            if loop._is_interrupted(view.run.run_id) or loop._is_cancelled(view.run.run_id):
                raise loop._interrupted_error("model_loop_interrupted") from exc
            if is_context_overflow(exc) and not overflow_retry:
                overflow_retry = 1
                continue
            if overflow_retry:
                break
            if attempt < loop.max_retries:
                attempt += 1
                continue
            break
        finally:
            loop._track(loop._active_requests, view.run.run_id, request.request_id, add=False)
    raise loop._loop_error(f"model_provider_retries_exhausted:{last_error}") from last_error
