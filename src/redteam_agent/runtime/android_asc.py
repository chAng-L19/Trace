from __future__ import annotations

"""Lazy APK/DEX analysis inspired by ASC's read-only database model.

This module intentionally keeps the APK in place.  The ZIP central directory is
used as the index, DEX entries are inflated only for a requested query, and
class/method references are resolved from the DEX tables on demand.  It is a
clean-room implementation of the approach, not a vendored copy of ASC.
"""

import hashlib
import struct
import time
import zipfile
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence


MAX_DEX_BYTES = 256 * 1024 * 1024
MAX_RESULTS = 500
_U16 = struct.Struct("<H")
_U32 = struct.Struct("<I")
_DEX_MAGICS = tuple(f"dex\n{version}\0".encode("ascii") for version in ("035", "037", "038", "039", "040", "041"))


def _u16(data: memoryview, offset: int) -> int:
    return _U16.unpack_from(data, offset)[0]


def _u32(data: memoryview, offset: int) -> int:
    return _U32.unpack_from(data, offset)[0]


def _uleb(data: memoryview, offset: int) -> tuple[int, int]:
    value = 0
    shift = 0
    for _ in range(5):
        if offset >= len(data):
            raise ValueError("dex_uleb_out_of_range")
        byte = int(data[offset])
        offset += 1
        value |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return value, offset
        shift += 7
    raise ValueError("dex_uleb_too_long")


def _bounded_int(value: Any, default: int, minimum: int, maximum: int) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError, OverflowError):
        parsed = default
    return max(minimum, min(maximum, parsed))


def _descriptor(value: str) -> str:
    text = str(value or "").strip()
    if not text:
        return text
    if text.startswith("L") and text.endswith(";"):
        return text
    return f"L{text.replace('.', '/').strip(';')};"


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _display_descriptor(value: str) -> str:
    text = str(value or "")
    if text.startswith("L") and text.endswith(";"):
        return text[1:-1].replace("/", ".")
    return text


@dataclass(frozen=True)
class ApkEntry:
    name: str
    compressed_size: int
    file_size: int
    compression: str
    crc32: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "compressed_size": self.compressed_size,
            "file_size": self.file_size,
            "compression": self.compression,
            "crc32": self.crc32,
        }


@dataclass(frozen=True)
class _Field:
    index: int
    owner: str
    name: str
    type_descriptor: str
    access_flags: int

    def display(self) -> str:
        return f"{self.owner}->{self.name}:{self.type_descriptor}"


@dataclass(frozen=True)
class _Method:
    index: int
    owner: str
    name: str
    prototype: str
    access_flags: int
    code_offset: int
    code_units: tuple[int, ...]

    def display(self) -> str:
        return f"{self.owner}->{self.name}{self.prototype}"


@dataclass(frozen=True)
class _Class:
    index: int
    descriptor: str
    access_flags: int
    superclass: str
    fields: tuple[_Field, ...]
    methods: tuple[_Method, ...]


class _DexView:
    """Validated, lazy view over one DEX buffer."""

    def __init__(self, name: str, data: bytes) -> None:
        self.name = name
        self.data = memoryview(data)
        if len(self.data) < 0x70 or bytes(self.data[:8]) not in _DEX_MAGICS:
            raise ValueError("dex_magic_invalid")
        self.file_size = _u32(self.data, 0x20)
        if self.file_size > len(self.data) or self.file_size < 0x70:
            raise ValueError("dex_file_size_invalid")
        self._strings: dict[int, str] = {}
        self._types: dict[int, str] = {}
        self._protos: dict[int, str] = {}
        self._fields: dict[int, _Field] = {}
        self._methods: dict[int, tuple[str, str, str]] = {}
        self._classes: tuple[_Class, ...] | None = None

    def _table(self, size_offset: int, offset_offset: int, item_size: int) -> tuple[int, int]:
        count = _u32(self.data, size_offset)
        offset = _u32(self.data, offset_offset)
        end = offset + count * item_size
        if count and (offset < 0x70 or end > len(self.data)):
            raise ValueError("dex_table_out_of_range")
        return count, offset

    def string(self, index: int) -> str:
        if index in self._strings:
            return self._strings[index]
        count, offset = self._table(0x38, 0x3C, 4)
        if not 0 <= index < count:
            raise ValueError("dex_string_index_invalid")
        string_offset = _u32(self.data, offset + index * 4)
        if string_offset >= len(self.data):
            raise ValueError("dex_string_offset_invalid")
        _, cursor = _uleb(self.data, string_offset)
        end = cursor
        while end < len(self.data) and self.data[end] != 0:
            end += 1
        if end >= len(self.data):
            raise ValueError("dex_string_unterminated")
        value = self.data[cursor:end].tobytes().decode("utf-8", errors="replace")
        self._strings[index] = value
        return value

    def type(self, index: int) -> str:
        if index in self._types:
            return self._types[index]
        count, offset = self._table(0x40, 0x44, 4)
        if not 0 <= index < count:
            raise ValueError("dex_type_index_invalid")
        value = self.string(_u32(self.data, offset + index * 4))
        self._types[index] = value
        return value

    def proto(self, index: int) -> str:
        if index in self._protos:
            return self._protos[index]
        count, offset = self._table(0x48, 0x4C, 12)
        if not 0 <= index < count:
            raise ValueError("dex_proto_index_invalid")
        return_type = self.type(_u32(self.data, offset + index * 12 + 4))
        params_offset = _u32(self.data, offset + index * 12 + 8)
        params: list[str] = []
        if params_offset:
            if params_offset + 4 > len(self.data):
                raise ValueError("dex_type_list_offset_invalid")
            size = _u32(self.data, params_offset)
            end = params_offset + 4 + size * 2
            if end > len(self.data):
                raise ValueError("dex_type_list_out_of_range")
            params = [self.type(_u16(self.data, params_offset + 4 + i * 2)) for i in range(size)]
        value = f"({''.join(params)}){return_type}"
        self._protos[index] = value
        return value

    def field(self, index: int) -> _Field:
        if index in self._fields:
            return self._fields[index]
        count, offset = self._table(0x50, 0x54, 8)
        if not 0 <= index < count:
            raise ValueError("dex_field_index_invalid")
        row = offset + index * 8
        item = _Field(index, self.type(_u16(self.data, row)), self.string(_u32(self.data, row + 4)), self.type(_u16(self.data, row + 2)), 0)
        self._fields[index] = item
        return item

    def method_ref(self, index: int) -> tuple[str, str, str]:
        if index in self._methods:
            return self._methods[index]
        count, offset = self._table(0x58, 0x5C, 8)
        if not 0 <= index < count:
            raise ValueError("dex_method_index_invalid")
        row = offset + index * 8
        value = (self.type(_u16(self.data, row)), self.string(_u32(self.data, row + 4)), self.proto(_u16(self.data, row + 2)))
        self._methods[index] = value
        return value

    def _code(self, code_offset: int) -> tuple[int, ...]:
        if not code_offset:
            return ()
        if code_offset + 16 > len(self.data):
            raise ValueError("dex_code_item_invalid")
        size = _u32(self.data, code_offset + 12)
        end = code_offset + 16 + size * 2
        if end > len(self.data):
            raise ValueError("dex_insns_out_of_range")
        return tuple(_u16(self.data, code_offset + 16 + i * 2) for i in range(size))

    def _class_data(self, offset: int, owner: str) -> tuple[tuple[_Field, ...], tuple[_Method, ...]]:
        if not offset:
            return (), ()
        if offset >= len(self.data):
            raise ValueError("dex_class_data_invalid")
        static_count, cursor = _uleb(self.data, offset)
        instance_count, cursor = _uleb(self.data, cursor)
        direct_count, cursor = _uleb(self.data, cursor)
        virtual_count, cursor = _uleb(self.data, cursor)
        fields: list[_Field] = []
        field_index = 0
        for _ in range(static_count + instance_count):
            diff, cursor = _uleb(self.data, cursor)
            access, cursor = _uleb(self.data, cursor)
            field_index += diff
            base = self.field(field_index)
            fields.append(_Field(base.index, owner, base.name, base.type_descriptor, access))
        methods: list[_Method] = []
        method_index = 0
        for _ in range(direct_count + virtual_count):
            diff, cursor = _uleb(self.data, cursor)
            access, cursor = _uleb(self.data, cursor)
            code_offset, cursor = _uleb(self.data, cursor)
            method_index += diff
            ref_owner, name, prototype = self.method_ref(method_index)
            methods.append(_Method(method_index, owner or ref_owner, name, prototype, access, code_offset, self._code(code_offset)))
        return tuple(fields), tuple(methods)

    def _class_at(self, index: int, offset: int | None = None) -> _Class:
        if offset is None:
            _, offset = self._table(0x60, 0x64, 32)
        row = offset + index * 32
        descriptor = self.type(_u32(self.data, row))
        superclass_index = _u32(self.data, row + 8)
        superclass = self.type(superclass_index) if superclass_index != 0xFFFFFFFF else ""
        fields, methods = self._class_data(_u32(self.data, row + 24), descriptor)
        return _Class(index, descriptor, _u32(self.data, row + 4), superclass, fields, methods)

    def _find_class(self, query: str) -> _Class | None:
        target = _descriptor(query)
        count, offset = self._table(0x60, 0x64, 32)
        for index in range(count):
            row = offset + index * 32
            if self.type(_u32(self.data, row)) == target:
                return self._class_at(index, offset)
        return None

    def classes(self) -> tuple[_Class, ...]:
        if self._classes is not None:
            return self._classes
        count, offset = self._table(0x60, 0x64, 32)
        classes = [self._class_at(index, offset) for index in range(count)]
        self._classes = tuple(classes)
        return self._classes

    def strings_matching(self, query: str, limit: int) -> list[tuple[int, str]]:
        count, _ = self._table(0x38, 0x3C, 4)
        needle = str(query or "").casefold()
        return [(index, self.string(index)) for index in range(count) if needle in self.string(index).casefold()][:limit]

    @staticmethod
    def _width(opcode: int) -> int:
        if 0x52 <= opcode <= 0x6D:
            return 2
        if 0x6E <= opcode <= 0x72 or 0x74 <= opcode <= 0x78:
            return 3
        if opcode in {0xFA, 0xFB}:
            return 4
        if opcode in {0x1B, 0x24, 0x25, 0x26, 0x2B, 0x2C}:
            return 3
        if opcode in {0x1A, 0x1C, 0x1F, 0x20, 0x22, 0x23}:
            return 2
        if opcode in {0x02, 0x03, 0x05, 0x06, 0x07, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x21, 0x2D, 0x2E, 0x2F, 0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39}:
            return 2
        if opcode in {0x09, 0x0E, 0x10, 0x11, 0x12, 0x1D, 0x1E}:
            return 1
        return 1

    def _references(self, method: _Method, kind: str, target_indexes: set[int]) -> list[dict[str, Any]]:
        hits: list[dict[str, Any]] = []
        units = method.code_units
        cursor = 0
        while cursor < len(units):
            opcode = units[cursor] & 0xFF
            width = min(self._width(opcode), len(units) - cursor)
            index: int | None = None
            if kind == "string" and opcode == 0x1A and width >= 2:
                index = units[cursor + 1]
            elif kind == "string" and opcode == 0x1B and width >= 3:
                index = units[cursor + 1] | (units[cursor + 2] << 16)
            elif kind == "type" and opcode in {0x1C, 0x1F, 0x20, 0x22, 0x23, 0x24, 0x25} and width >= 2:
                index = units[cursor + 1]
            elif kind == "field" and 0x52 <= opcode <= 0x6D and width >= 2:
                index = units[cursor + 1]
            elif kind == "method" and (0x6E <= opcode <= 0x72 or 0x74 <= opcode <= 0x78) and width >= 2:
                index = units[cursor + 1]
            elif kind == "method" and opcode in {0xFA, 0xFB} and width >= 3:
                index = units[cursor + 1]
            if index is not None and index in target_indexes:
                hits.append({"method": method.display(), "offset_units": cursor, "opcode": f"0x{opcode:02x}", "index": index})
            cursor += max(1, width)
        return hits

    def findrefs(self, kind: str, query: str, class_query: str = "", fuzzy_class: bool = False, limit: int = MAX_RESULTS) -> list[dict[str, Any]]:
        kind = str(kind).casefold().strip()
        if kind not in {"string", "type", "method", "field"}:
            raise ValueError("apk_findrefs_type_invalid")
        normalized_class = _descriptor(class_query) if class_query and not fuzzy_class else str(class_query or "").casefold()
        targets: set[int] = set()
        labels: dict[int, str] = {}
        if kind == "string":
            for index, value in self.strings_matching(query, limit):
                targets.add(index)
                labels[index] = value
        elif kind == "type":
            count, _ = self._table(0x40, 0x44, 4)
            needle = str(query or "").casefold().replace(".", "/")
            for index in range(count):
                value = self.type(index)
                if needle in value.casefold().replace(".", "/"):
                    targets.add(index)
                    labels[index] = value
        elif kind == "method":
            count, _ = self._table(0x58, 0x5C, 8)
            needle = str(query or "").casefold()
            for index in range(count):
                owner, name, prototype = self.method_ref(index)
                class_match = (normalized_class in owner if fuzzy_class else not class_query or owner == normalized_class)
                if class_match and needle in name.casefold():
                    targets.add(index)
                    labels[index] = f"{owner}->{name}{prototype}"
        else:
            count, _ = self._table(0x50, 0x54, 8)
            needle = str(query or "").casefold()
            for index in range(count):
                field = self.field(index)
                class_match = (normalized_class in field.owner if fuzzy_class else not class_query or field.owner == normalized_class)
                if class_match and needle in field.name.casefold():
                    targets.add(index)
                    labels[index] = field.display()
        results: list[dict[str, Any]] = []
        for clazz in self.classes():
            if kind == "type" and any(index in targets and self.type(index) == clazz.descriptor for index in targets):
                results.append({"dex": self.name, "kind": "class", "class": clazz.descriptor, "match": clazz.descriptor})
            for method in clazz.methods:
                for hit in self._references(method, kind, targets):
                    hit.update({"dex": self.name, "kind": "reference", "class": clazz.descriptor, "match": labels.get(hit["index"], "")})
                    results.append(hit)
            if kind in {"method", "field"}:
                declarations = clazz.methods if kind == "method" else clazz.fields
                for item in declarations:
                    if item.index in targets:
                        result = {"dex": self.name, "kind": "declaration", "class": clazz.descriptor, "match": labels[item.index]}
                        if kind == "method":
                            result["method"] = item.display()
                        else:
                            result["field"] = item.display()
                        results.append(result)
            if len(results) >= limit:
                return results[:limit]
        if kind == "string" and targets and not results:
            return [{"dex": self.name, "kind": "string_table", "match": labels[index], "index": index} for index in sorted(targets)[:limit]]
        return results[:limit]

    def getclass(self, query: str, max_methods: int = 200) -> dict[str, Any] | None:
        clazz = self._find_class(query)
        if clazz is None:
            return None
        methods = list(clazz.methods[:max_methods])
        lines = [f"class {_display_descriptor(clazz.descriptor)}", "{" ]
        if clazz.superclass:
            lines[0] += f" extends {_display_descriptor(clazz.superclass)}"
        for field in clazz.fields:
            lines.append(f"  field {field.display()};")
        for method in methods:
            lines.append(f"  method {method.name}{method.prototype} // code_units={len(method.code_units)}")
            if method.code_units:
                lines.append("    bytecode " + " ".join(f"{unit:04x}" for unit in method.code_units[:64]))
        if len(clazz.methods) > len(methods):
            lines.append(f"  // methods_truncated={len(clazz.methods) - len(methods)}")
        lines.append("}")
        return {
            "dex": self.name,
            "class": clazz.descriptor,
            "superclass": clazz.superclass,
            "fields": [field.display() for field in clazz.fields],
            "methods": [method.display() for method in methods],
            "source_kind": "targeted-structural-pseudocode",
            "source": "\n".join(lines),
        }


class ApkAscEngine:
    def __init__(self, path: Path, *, max_dex_bytes: int = MAX_DEX_BYTES, workers: int = 4) -> None:
        self.path = path.expanduser().resolve(strict=True)
        if not self.path.is_file():
            raise ValueError("apk_path_must_be_file")
        self.max_dex_bytes = _bounded_int(max_dex_bytes, MAX_DEX_BYTES, 1 << 20, MAX_DEX_BYTES)
        self.workers = _bounded_int(workers, 4, 1, 16)

    def _entries(self) -> tuple[ApkEntry, ...]:
        with zipfile.ZipFile(self.path) as archive:
            return tuple(
                ApkEntry(item.filename, item.compress_size, item.file_size, "stored" if item.compress_type == zipfile.ZIP_STORED else "deflated" if item.compress_type == zipfile.ZIP_DEFLATED else f"method-{item.compress_type}", f"{item.CRC:08x}")
                for item in archive.infolist()
            )

    def _dex_names(self) -> tuple[str, ...]:
        return tuple(item.name for item in self._entries() if item.name == "classes.dex" or (item.name.startswith("classes") and item.name.endswith(".dex") and item.name[7:-4].isdigit()))

    def _read_dex(self, name: str) -> _DexView:
        with zipfile.ZipFile(self.path) as archive:
            info = archive.getinfo(name)
            if info.file_size > self.max_dex_bytes:
                raise ValueError("dex_entry_too_large")
            data = archive.read(info)
        return _DexView(name, data)

    def inventory(self) -> dict[str, Any]:
        entries = self._entries()
        dex_entries = [item for item in entries if item.name == "classes.dex" or (item.name.startswith("classes") and item.name.endswith(".dex"))]
        libraries = [item.name for item in entries if item.name.startswith("lib/") and item.name.endswith(".so")]
        assets = [item.name for item in entries if item.name.startswith("assets/")]
        indicators: list[str] = []
        lowered = {item.name.casefold() for item in entries}
        if libraries:
            indicators.append("native_loader_present")
        if len(dex_entries) > 1:
            indicators.append("multi_dex")
        if any(item.compression != "stored" for item in dex_entries):
            indicators.append("compressed_dex")
        if not any(item.name == "classes.dex" for item in dex_entries):
            indicators.append("primary_dex_missing")
        if any(name.endswith((".dex", ".odex", ".vdex")) for name in assets):
            indicators.append("dynamic_dex_asset_candidate")
        packer_markers = ("jiagu", "secneo", "shell", "protect", "ijiami", "dexhelper")
        matched_markers = sorted({marker for marker in packer_markers if any(marker in name for name in lowered)})
        indicators.extend(f"packer_marker:{marker}" for marker in matched_markers)
        dex_headers: list[dict[str, Any]] = []
        with zipfile.ZipFile(self.path) as archive:
            for item in dex_entries:
                try:
                    header = archive.open(item.name).read(8)
                    valid = header in _DEX_MAGICS
                except (OSError, KeyError, RuntimeError, zipfile.BadZipFile):
                    header = b""
                    valid = False
                dex_headers.append({"name": item.name, "magic": header.hex(), "valid": valid})
                if not valid:
                    indicators.append(f"invalid_dex_header:{item.name}")
        return {
            "engine": "trace-asc",
            "strategy": "lazy-apk-database",
            "path": str(self.path),
            "sha256": _sha256_file(self.path),
            "entry_count": len(entries),
            "dex_count": len(dex_entries),
            "dex_entries": [item.to_dict() for item in dex_entries],
            "dex_headers": dex_headers,
            "native_libraries": libraries[:MAX_RESULTS],
            "asset_candidates": [name for name in assets if name.endswith((".dex", ".odex", ".vdex"))][:MAX_RESULTS],
            "protection_indicators": sorted(set(indicators)),
            "entries": [item.to_dict() for item in entries[:MAX_RESULTS]],
        }

    def findrefs(self, kind: str, query: str, *, class_query: str = "", fuzzy_class: bool = False, limit: int = MAX_RESULTS) -> dict[str, Any]:
        names = self._dex_names()
        started = time.perf_counter()

        def query_one(name: str) -> dict[str, Any]:
            try:
                view = self._read_dex(name)
                return {"dex": name, "matches": view.findrefs(kind, query, class_query, fuzzy_class, limit), "error": ""}
            except (OSError, ValueError) as exc:
                return {"dex": name, "matches": [], "error": type(exc).__name__ + ":" + str(exc)}

        with ThreadPoolExecutor(max_workers=min(self.workers, max(1, len(names)))) as pool:
            per_dex = list(pool.map(query_one, names))
        matches = [match for item in per_dex for match in item["matches"]][:limit]
        return {
            "engine": "trace-asc",
            "strategy": "lazy-cross-dex-findrefs",
            "path": str(self.path),
            "query": {"type": kind, "value": query, "class": class_query, "fuzzy_class": fuzzy_class},
            "dex_count": len(names),
            "matches": matches,
            "count": len(matches),
            "truncated": len(matches) >= limit,
            "dex_errors": [item for item in per_dex if item["error"]],
            "elapsed_ms": round((time.perf_counter() - started) * 1000, 3),
        }

    def getclass(self, query: str, *, max_methods: int = 200) -> dict[str, Any]:
        started = time.perf_counter()
        for name in self._dex_names():
            try:
                result = self._read_dex(name).getclass(query, max_methods=max_methods)
            except (OSError, ValueError):
                result = None
            if result is not None:
                result["engine"] = "trace-asc"
                result["strategy"] = "lazy-targeted-class"
                result["path"] = str(self.path)
                result["elapsed_ms"] = round((time.perf_counter() - started) * 1000, 3)
                return result
        raise ValueError(f"apk_class_not_found:{_descriptor(query)}")


def apk_asc(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    raw_path = str(arguments.get("path") or arguments.get("target") or "").strip()
    if not raw_path:
        raise ValueError("apk_path_required")
    engine = ApkAscEngine(
        Path(raw_path),
        max_dex_bytes=arguments.get("max_dex_bytes", MAX_DEX_BYTES),
        workers=arguments.get("workers", 4),
    )
    operation = str(arguments.get("operation") or "inventory").casefold().strip()
    if operation in {"inventory", "protection", "triage"}:
        result = engine.inventory()
        result["operation"] = operation
        return result
    if operation == "getclass":
        return engine.getclass(str(arguments.get("class_name") or arguments.get("query") or ""), max_methods=_bounded_int(arguments.get("max_methods"), 200, 1, MAX_RESULTS))
    if operation == "findrefs":
        return engine.findrefs(
            str(arguments.get("query_type") or arguments.get("find_type") or "string"),
            str(arguments.get("query") or arguments.get("value") or ""),
            class_query=str(arguments.get("class_name") or ""),
            fuzzy_class=bool(arguments.get("fuzzy_class", False)),
            limit=_bounded_int(arguments.get("max_results"), 100, 1, MAX_RESULTS),
        )
    raise ValueError("apk_operation_invalid")


__all__ = ["ApkAscEngine", "apk_asc"]
