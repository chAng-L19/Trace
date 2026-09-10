from __future__ import annotations

from typing import Any


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
        request = loop._request(
            view,
            attempt=attempt,
            force_compaction=bool(overflow_retry),
            overflow_retry=overflow_retry,
        )
        loop._save_request(request)
        loop._track(loop._active_requests, view.run.run_id, request.request_id, add=True)
        try:
            response = loop._invoke(request)
            validated = loop._validate_response(request, response)
            return validated, request
        except loop._integrity_error:
            raise
        except BaseException as exc:
            last_error = exc
            loop._save_failure_response(request, exc)
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
