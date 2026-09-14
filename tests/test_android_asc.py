from __future__ import annotations

import struct
import zipfile
from pathlib import Path

from redteam_agent.runtime.android_asc import ApkAscEngine, apk_asc
from redteam_agent.runtime.operation_runtime import OperationRuntime


def _uleb(value: int) -> bytes:
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _minimal_dex() -> bytes:
    strings = ["Lcom/example/Main;", "Ljava/lang/Object;", "V", "main", "token"]
    data = bytearray(b"\0" * 0x70)
    string_ids_off = len(data)
    data.extend(b"\0" * (len(strings) * 4))
    type_ids_off = len(data)
    data.extend(b"\0" * (3 * 4))
    proto_ids_off = len(data)
    data.extend(b"\0" * 12)
    method_ids_off = len(data)
    data.extend(b"\0" * 8)
    class_defs_off = len(data)
    data.extend(b"\0" * 32)
    while len(data) % 4:
        data.append(0)
    code_off = len(data)
    data.extend(struct.pack("<HHHHII", 1, 0, 0, 0, 0, 2))
    data.extend(struct.pack("<HH", 0x001A, 4))
    data.extend(struct.pack("<H", 0x000E))
    class_data_off = len(data)
    data.extend(_uleb(0) + _uleb(0) + _uleb(1) + _uleb(0))
    data.extend(_uleb(0) + _uleb(0x9) + _uleb(code_off))
    string_offsets: list[int] = []
    for value in strings:
        string_offsets.append(len(data))
        encoded = value.encode("utf-8")
        data.extend(_uleb(len(value)) + encoded + b"\0")
    for index, offset in enumerate(string_offsets):
        struct.pack_into("<I", data, string_ids_off + index * 4, offset)
    for index, string_index in enumerate((0, 1, 2)):
        struct.pack_into("<I", data, type_ids_off + index * 4, string_index)
    struct.pack_into("<III", data, proto_ids_off, 2, 2, 0)
    struct.pack_into("<HHI", data, method_ids_off, 0, 0, 3)
    struct.pack_into("<8I", data, class_defs_off, 0, 1, 1, 0, 0xFFFFFFFF, 0, class_data_off, 0)
    struct.pack_into("<8s", data, 0, b"dex\n035\0")
    struct.pack_into("<I", data, 0x20, len(data))
    struct.pack_into("<I", data, 0x24, 0x70)
    struct.pack_into("<I", data, 0x28, 0x12345678)
    struct.pack_into("<I", data, 0x38, len(strings))
    struct.pack_into("<I", data, 0x3C, string_ids_off)
    struct.pack_into("<I", data, 0x40, 3)
    struct.pack_into("<I", data, 0x44, type_ids_off)
    struct.pack_into("<I", data, 0x48, 1)
    struct.pack_into("<I", data, 0x4C, proto_ids_off)
    struct.pack_into("<I", data, 0x50, 0)
    struct.pack_into("<I", data, 0x54, 0)
    struct.pack_into("<I", data, 0x58, 1)
    struct.pack_into("<I", data, 0x5C, method_ids_off)
    struct.pack_into("<I", data, 0x60, 1)
    struct.pack_into("<I", data, 0x64, class_defs_off)
    struct.pack_into("<I", data, 0x68, len(data) - 0x70)
    struct.pack_into("<I", data, 0x6C, 0x70)
    return bytes(data)


def _apk(tmp_path: Path) -> Path:
    path = tmp_path / "fixture.apk"
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr("classes.dex", _minimal_dex(), compress_type=zipfile.ZIP_STORED)
        archive.writestr("classes2.dex", b"not-a-dex", compress_type=zipfile.ZIP_DEFLATED)
        archive.writestr("lib/arm64-v8a/libprotect.so", b"native")
        archive.writestr("assets/payload.dex", b"payload")
    return path


def test_asc_inventory_is_lazy_and_protection_aware(tmp_path: Path) -> None:
    result = ApkAscEngine(_apk(tmp_path)).inventory()
    assert result["engine"] == "trace-asc"
    assert result["dex_count"] == 2
    assert "native_loader_present" in result["protection_indicators"]
    assert "compressed_dex" in result["protection_indicators"]
    assert "dynamic_dex_asset_candidate" in result["protection_indicators"]


def test_asc_findrefs_and_targeted_class(tmp_path: Path) -> None:
    path = _apk(tmp_path)
    refs = apk_asc({"path": str(path), "operation": "findrefs", "query_type": "string", "query": "token"})
    assert refs["count"] == 1
    assert refs["matches"][0]["kind"] == "reference"
    target = apk_asc({"path": str(path), "operation": "getclass", "class_name": "com.example.Main"})
    assert target["source_kind"] == "targeted-structural-pseudocode"
    assert "main()V" in target["source"]


def test_asc_is_registered_as_a_builtin_tool(tmp_path: Path) -> None:
    runtime = OperationRuntime(root=tmp_path / "runtime")
    descriptor = next(item for item in runtime.broker.descriptors() if item.qualified_name == "builtin:apk-asc")
    assert {"apk_decompile", "android_static_analysis"} <= set(descriptor.capabilities)
    runtime.broker.close()
