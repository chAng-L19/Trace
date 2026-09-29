from __future__ import annotations

from collections.abc import Mapping
from typing import Any


_PLAINTEXT = {"reasoning", "reasoning_content", "chain_of_thought", "thinking", "analysis", "summary", "text", "content"}
_OPAQUE = {"encrypted_content", "thought_signature", "thoughtSignature", "signature", "continuation_token"}


def opaque_only(value: Any) -> Any:
    """Protocol state is opaque; never persist providers' plaintext reasoning."""
    if isinstance(value, Mapping):
        if value.get("type") in {"reasoning.text", "reasoning.summary"}:
            return {}
        return {key: opaque_only(item) for key, item in value.items() if key not in _PLAINTEXT}
    if isinstance(value, (tuple, list)):
        return [opaque_only(item) for item in value]
    return value


def chat_continuation(message: Mapping[str, Any]) -> dict[str, Any]:
    result = {key: value for key, value in message.items() if key in _OPAQUE}
    details = message.get("reasoning_details")
    if isinstance(details, list):
        encrypted = [opaque_only(item) for item in details if isinstance(item, Mapping)
                     and (item.get("type") == "reasoning.encrypted" or item.get("encrypted_content"))]
        if encrypted:
            result["reasoning_details"] = encrypted
    calls = []
    for call in message.get("tool_calls") or ():
        signature = {key: value for key, value in call.items() if key in _OPAQUE}
        extra = call.get("extra_content")
        if isinstance(extra, Mapping):
            google = extra.get("google")
            if isinstance(google, Mapping) and google.get("thought_signature"):
                signature["extra_content"] = {"google": {"thought_signature": google["thought_signature"]}}
        if signature:
            calls.append({"id": call.get("id"), **signature})
    if calls:
        result["tool_calls"] = calls
    return opaque_only(result)


def merge_opaque_delta(target: dict, delta: Mapping) -> None:
    for key, value in delta.items():
        if key in _OPAQUE | {"data"} and isinstance(value, str):
            target[key] = str(target.get(key) or "") + value
        elif isinstance(value, Mapping):
            merge_opaque_delta(target.setdefault(key, {}), value)
        elif key == "reasoning_details" and isinstance(value, list):
            saved = target.setdefault(key, [])
            for index, item in enumerate(value):
                identity = item.get("index", index)
                previous = next((entry for pos, entry in enumerate(saved)
                                 if entry.get("index", pos) == identity), None)
                if previous is None:
                    saved.append(dict(item))
                else:
                    merge_opaque_delta(previous, item)
        else:
            target[key] = value
