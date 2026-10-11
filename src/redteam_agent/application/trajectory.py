from __future__ import annotations

"""User-facing projection of Trace's durable run into a Harness trajectory."""

from collections.abc import Mapping
from typing import Any
from urllib.parse import urlsplit

from ..runtime.security import redact_sensitive
from .tool_projection import ToolObservationProjector


def text_summary(value: Any, limit: int = 280) -> str:
    if value is None or isinstance(value, (Mapping, list, tuple)):
        return ""
    # Redact before truncating, so a credential crossing the cutoff stays hidden.
    text = str(redact_sensitive(value)).replace("\r", " ").replace("\n", " ").strip()
    return text if len(text) <= limit else text[: limit - 1] + "…"


def tool_request_summary(tool_name: str, arguments: Mapping[str, Any]) -> dict[str, Any]:
    target = arguments.get("url") or arguments.get("target") or arguments.get("host") or ""
    path = arguments.get("path") or ""
    if isinstance(target, str) and target.startswith(("http://", "https://")):
        try:
            parsed = urlsplit(target)
            path = path or (parsed.path or "/") + ("?" + parsed.query if parsed.query else "")
            target = f"{parsed.scheme}://{parsed.netloc}"
        except ValueError:
            pass  # Credential references can make userinfo unsuitable for URL parsing.
    method = arguments.get("method") or ("GET" if tool_name in {"builtin:http-request", "builtin:page-fetch"} else "")
    return {"target": text_summary(target), "method": text_summary(method, 32), "path": text_summary(path)}


def tool_result_summary(output: Any) -> dict[str, Any]:
    output = redact_sensitive(output)
    result = {}
    if isinstance(output, Mapping):
        for key, candidates in (("http_status", ("status_code", "http_status")),
                                ("exit_code", ("exit_code",)), ("error", ("error", "stderr"))):
            for candidate in candidates:
                value = ToolObservationProjector._find(output, (candidate,))
                if value not in (None, ""):
                    result[key] = text_summary(value)
                    break
        # Use semantic output fields, never stringify the entire tool envelope.
        value = None
        for key in ("summary", "text", "body", "response_body", "stdout", "message", "preview"):
            value = ToolObservationProjector._find(output, (key,))
            if value not in (None, ""):
                break
        if isinstance(value, Mapping) and "preview" in value:
            value = value["preview"]
        if value in (None, ""):
            value = output
    else:
        value = output
    if isinstance(value, Mapping):
        fields = [f"{text_summary(key, 60)}：{text_summary(item, 120)}" for key, item in value.items()
                  if item is not None and not isinstance(item, (Mapping, list, tuple))][:4]
        summary = "；".join(fields) or f"返回 {len(value)} 个字段"
    elif isinstance(value, (list, tuple)):
        summary = f"返回 {len(value)} 条结果"
    else:
        summary = value
    result["text"] = text_summary(summary, 600)
    return result


def _usage(value: Any) -> dict[str, int]:
    if not isinstance(value, Mapping):
        return {}
    return {key: value[key] for key in ("input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens")
            if isinstance(value.get(key), int) and not isinstance(value[key], bool)}


def _turn_id(payload: Mapping[str, Any]) -> str:
    return str(payload.get("turn_id") or payload.get("action_id") or payload.get("request_id") or "")


def _step(run_id: str, sequence: int, created_at: str, kind: str, title: str, **values: Any) -> dict[str, Any]:
    result = {
        "run_id": run_id,
        "sequence": sequence,
        "created_at": created_at,
        "kind": kind,
        "title": title,
    }
    result.update({key: value for key, value in values.items() if value not in (None, "", [], {})})
    return result


def project_event(event: Any) -> dict[str, Any] | None:
    """Convert an internal event into the Harness run/turn/step vocabulary."""
    payload = event.payload if isinstance(event.payload, Mapping) else {}
    event_type = str(event.event_type)
    common = {"run_id": event.run_id, "sequence": event.sequence, "created_at": event.created_at}
    if event_type == "operation_started":
        return _step(**common, kind="run_start", title="开始执行任务", status="running")
    if event_type == "harness_step_started":
        return _step(**common, kind="step_start", title="请求模型推演", turn_id=_turn_id(payload),
                     step_id=str(payload.get("request_id") or ""), attempt=int(payload.get("attempt", 0)) + 1,
                     model=text_summary(payload.get("model"), 120), provider=text_summary(payload.get("provider"), 80))
    if event_type == "harness_step_finished":
        usage = payload.get("usage")
        title = "模型完成推演" if payload.get("status") in {None, "", "completed", "success"} else "模型推演未完成"
        return _step(**common, kind="step_end", title=title, turn_id=_turn_id(payload),
                     step_id=str(payload.get("request_id") or ""), status=text_summary(payload.get("status"), 50),
                     finish_reason=text_summary(payload.get("finish_reason"), 50), tool_count=payload.get("tool_count"),
                     usage=_usage(usage), text=text_summary(payload.get("text"), 600),
                     error=text_summary(payload.get("error")))
    if event_type == "harness_step_failed":
        return _step(**common, kind="step_end", title="模型推演失败", turn_id=_turn_id(payload),
                     step_id=str(payload.get("request_id") or ""), status="failed",
                     error=text_summary(payload.get("error"), 180))
    if event_type == "harness_tool_started":
        return _step(**common, kind="tool_start", title="开始执行工具", turn_id=_turn_id(payload),
                     step_id=str(payload.get("request_id") or ""), tool_name=text_summary(payload.get("tool_name"), 120),
                     call_id=text_summary(payload.get("call_id"), 120),
                     target=text_summary(payload.get("target")), method=text_summary(payload.get("method"), 32),
                     path=text_summary(payload.get("path")))
    if event_type == "harness_tool_finished":
        return _step(**common, kind="tool_end", title="工具返回结果", turn_id=_turn_id(payload),
                     step_id=str(payload.get("request_id") or ""), tool_name=text_summary(payload.get("tool_name"), 120),
                     call_id=text_summary(payload.get("call_id"), 120), status=text_summary(payload.get("status"), 50),
                     error=text_summary(payload.get("error")), text=text_summary(payload.get("text"), 600),
                     http_status=text_summary(payload.get("http_status"), 16),
                     exit_code=text_summary(payload.get("exit_code"), 16))
    if event_type == "model_stream":
        stream_type = str(payload.get("event_type") or "")
        delta = payload.get("delta") if isinstance(payload.get("delta"), Mapping) else {}
        if stream_type in {"text", "text_delta"}:
            byte_count = delta.get("byte_count")
            detail = f"已收到 {byte_count} 字节文本" if byte_count is not None else "已收到文本增量"
            detail += "，等待本次模型响应完成"
            return _step(**common, kind="message_update", title="模型正在输出", turn_id=_turn_id(payload),
                         step_id=str(payload.get("request_id") or ""), text=detail, provisional=True)
        if stream_type == "status":
            status = text_summary(delta.get("status"), 60)
            if not status and delta.get("content_hash"):
                status = "状态已更新"
            return _step(**common, kind="step_status", title="模型状态更新", turn_id=_turn_id(payload),
                         step_id=str(payload.get("request_id") or ""), status=status, provisional=True)
        if stream_type == "usage":
            return _step(**common, kind="usage", title="已收到用量", turn_id=_turn_id(payload),
                         step_id=str(payload.get("request_id") or ""), usage=_usage(delta), provisional=True)
        if stream_type == "tool_call":
            tool_name = text_summary(delta.get("tool_name") or delta.get("name"), 120)
            if not tool_name:
                return None
            return _step(**common, kind="tool_start", title="模型请求工具", turn_id=_turn_id(payload),
                         step_id=str(payload.get("request_id") or ""), tool_name=tool_name, provisional=True)
    if event_type == "model_stream_status":
        status = text_summary(payload.get("status"), 60)
        return _step(**common, kind="step_status", title="流式推演状态", turn_id=_turn_id(payload), status=status)
    if event_type == "model_provider_recovery":
        return _step(**common, kind="retry", title="模型请求准备重试", turn_id=_turn_id(payload),
                     status=text_summary(payload.get("action"), 60), attempt=payload.get("attempt"),
                     reason=text_summary(payload.get("category"), 80))
    if event_type == "model_turn_consumed":
        return _step(**common, kind="turn_end", title="本轮推演完成", turn_id=_turn_id(payload),
                     status=text_summary(payload.get("reason"), 80))
    if event_type == "operation_completed":
        return _step(**common, kind="run_end", title="任务完成", status="completed")
    if event_type in {"operation_failed", "operation_integrity_failed"}:
        return _step(**common, kind="run_end", title="任务失败", status="failed",
                     reason=text_summary(payload.get("reason") or payload.get("error"), 180))
    if event_type in {"run_paused", "run_budget_paused"}:
        return _step(**common, kind="run_status", title="任务已暂停", status="paused",
                     reason=text_summary(payload.get("reason"), 180))
    return None
