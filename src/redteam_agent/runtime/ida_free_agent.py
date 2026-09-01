"""Read-only IDAPython endpoint loaded by IDA Free with ``-S``."""

from __future__ import annotations

import json
import os
import socket


def _connect() -> tuple[socket.socket, object]:
    connection = socket.create_connection(
        (
            os.environ["REDTEAM_IDA_FREE_BRIDGE_HOST"],
            int(os.environ["REDTEAM_IDA_FREE_BRIDGE_PORT"]),
        ),
        timeout=180,
    )
    connection.sendall(
        (
            json.dumps(
                {
                    "token": os.environ["REDTEAM_IDA_FREE_BRIDGE_TOKEN"],
                    "session_id": os.environ["REDTEAM_IDA_FREE_BRIDGE_SESSION"],
                },
                separators=(",", ":"),
            )
            + "\n"
        ).encode("utf-8")
    )
    return connection, connection.makefile("rwb", buffering=0)


def _write(stream: object, payload: object) -> None:
    stream.write((json.dumps(payload, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8"))
    stream.flush()


def _read(stream: object) -> object:
    line = stream.readline()
    if not line:
        raise EOFError
    return json.loads(line.decode("utf-8", errors="replace"))


def _ea(value: object) -> int:
    import idaapi
    import ida_name

    if isinstance(value, int):
        return value
    text = str(value).strip()
    try:
        return int(text, 0)
    except ValueError:
        address = ida_name.get_name_ea(idaapi.BADADDR, text)
        if address == idaapi.BADADDR:
            raise ValueError(f"address_not_found:{text}")
        return int(address)


def _function(value: object):
    import ida_funcs

    address = _ea(value)
    function = ida_funcs.get_func(address)
    if function is None:
        raise ValueError(f"function_not_found:{value}")
    return function


def _dispatch(tool: str, arguments: dict) -> object:
    import idc
    import idautils
    import ida_funcs
    import ida_name

    if tool == "server_health":
        auto_is_ok = getattr(idc, "auto_is_ok", lambda: True)
        return {"healthy": True, "input_path": idc.get_input_file_path(), "auto_analysis": bool(auto_is_ok())}
    if tool == "list_funcs":
        limit = max(1, min(int(arguments.get("limit", 200)), 5000))
        return {"functions": [{"address": hex(int(ea)), "name": ida_name.get_name(ea) or ""} for ea in list(idautils.Functions())[:limit]]}
    if tool == "imports":
        limit = max(1, min(int(arguments.get("limit", 500)), 5000))
        imports: list[dict] = []
        import ida_nalt

        for module_index in range(ida_nalt.get_import_module_qty()):
            module = ida_nalt.get_import_module_name(module_index) or ""
            def add_import(address, name, ordinal):
                if len(imports) < limit:
                    imports.append({"address": hex(int(address)), "module": module, "name": name or "", "ordinal": int(ordinal)})
                return len(imports) < limit
            ida_nalt.enum_import_names(module_index, add_import)
            if len(imports) >= limit:
                break
        return {"imports": imports}
    if tool == "decompile":
        function = _function(arguments.get("function"))
        import ida_hexrays

        if not ida_hexrays.init_hexrays_plugin():
            raise RuntimeError("hexrays_unavailable")
        result = ida_hexrays.decompile(function.start_ea)
        return {"address": hex(int(function.start_ea)), "name": ida_name.get_name(function.start_ea) or "", "code": str(result)}
    if tool == "disasm":
        function = _function(arguments.get("function"))
        limit = max(1, min(int(arguments.get("limit", 300)), 5000))
        lines = []
        for address in idautils.Heads(function.start_ea, function.end_ea):
            line = idc.generate_disasm_line(address, 0) or ""
            lines.append({"address": hex(int(address)), "text": line})
            if len(lines) >= limit:
                break
        return {"address": hex(int(function.start_ea)), "name": ida_name.get_name(function.start_ea) or "", "instructions": lines}
    if tool == "xrefs_to":
        address = _ea(arguments.get("target"))
        return {"target": hex(address), "xrefs": [{"from": hex(int(reference.frm)), "type": int(reference.type)} for reference in idautils.XrefsTo(address)]}
    if tool == "get_string":
        address = _ea(arguments.get("address"))
        value = idc.get_strlit_contents(address, -1, idc.STRTYPE_C)
        return {"address": hex(address), "value": value.decode("utf-8", errors="replace") if isinstance(value, bytes) else value}
    if tool == "get_bytes":
        address = _ea(arguments.get("address"))
        size = max(1, min(int(arguments.get("size", 64)), 1024 * 1024))
        import ida_bytes

        data = ida_bytes.get_bytes(address, size) or b""
        return {"address": hex(address), "size": len(data), "hex": data.hex()}
    if tool == "get_int":
        address = _ea(arguments.get("address"))
        width = int(arguments.get("width", 4))
        import ida_bytes

        readers = {1: ida_bytes.get_byte, 2: ida_bytes.get_word, 4: ida_bytes.get_dword, 8: ida_bytes.get_qword}
        if width not in readers:
            raise ValueError("width_must_be_1_2_4_or_8")
        return {"address": hex(address), "width": width, "value": int(readers[width](address))}
    if tool == "shutdown":
        if arguments.get("save"):
            try:
                import ida_loader

                ida_loader.save_database(None, 0)
            except Exception:
                pass
        import idaapi

        return {"closed": True}
    if tool == "cancel":
        return {"cancelled": True}
    raise ValueError(f"ida_free_tool_not_supported:{tool}")


_STREAM = None
_REQUEST_ID = ""


def main() -> None:
    global _STREAM, _REQUEST_ID
    connection, stream = _connect()
    _STREAM = stream
    try:
        while True:
            request = _read(stream)
            _REQUEST_ID = str(request.get("id") or "")
            try:
                tool = str(request.get("tool") or "")
                result = _dispatch(tool, dict(request.get("arguments") or {}))
                _write(stream, {"id": _REQUEST_ID, "result": result})
                if tool == "shutdown":
                    import idaapi

                    idaapi.qexit(0)
                    break
            except Exception as exc:
                _write(stream, {"id": _REQUEST_ID, "error": str(exc)})
    except (EOFError, OSError):
        pass
    finally:
        try:
            stream.close()
            connection.close()
        except Exception:
            pass


main()
