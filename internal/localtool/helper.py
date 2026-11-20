"""One JSON input per process, embedded by Trace and invoked with python -c."""

import ast
import hashlib
import importlib.metadata
import importlib.util
import json
import os
import pathlib
import re
import shutil
import socket
import struct
import sys
import time
import urllib.parse
from email.message import Message
from html.parser import HTMLParser


def _frida_device(args):
    import frida
    device_id = args.get("device_id")
    if device_id:
        return frida.get_device(device_id)
    return frida.get_local_device()


def frida_tool(args):
    """Run one bounded Frida operation and always release its session."""
    import frida

    action = args.get("action", "devices")
    if action == "devices":
        return {"action": action, "devices": [
            {"id": item.id, "name": item.name, "type": item.type}
            for item in frida.enumerate_devices()
        ]}
    device = _frida_device(args)
    if action == "processes":
        return {"action": action, "device": device.id, "processes": [
            {"pid": item.pid, "name": item.name}
            for item in device.enumerate_processes()
        ]}
    if action != "instrument":
        raise ValueError("action must be devices, processes or instrument")
    source = args.get("script")
    if not isinstance(source, str) or not source.strip():
        raise ValueError("script is required for instrument")
    duration = args.get("duration_ms", 1000)
    if not isinstance(duration, int) or isinstance(duration, bool) or duration < 0:
        raise ValueError("duration_ms must be a non-negative integer")
    pid = args.get("pid")
    if pid is None and args.get("process"):
        process = device.get_process(args["process"])
        pid = process.pid
    if not isinstance(pid, int) or isinstance(pid, bool) or pid < 1:
        raise ValueError("instrument requires a positive pid or process name")
    session = None
    script = None
    messages = []
    errors = []

    def on_message(message, data):
        item = dict(message)
        if data is not None:
            item["data"] = bytes(data).hex()
        messages.append(item)
        if message.get("type") == "error":
            errors.append({"stage": "script", "error": message})

    result = {"action": action, "device": device.id, "pid": pid,
              "messages": messages, "errors": errors,
              "detached": False}
    try:
        session = device.attach(pid)
        script = session.create_script(source)
        script.on("message", on_message)
        script.load()
        time.sleep(duration / 1000)
    except Exception as exc:
        errors.append({"stage": "instrument", "error": str(exc)})
    finally:
        if script is not None:
            try:
                script.unload()
            except Exception as exc:
                errors.append({"stage": "unload", "error": str(exc)})
        if session is not None:
            try:
                session.detach()
                result["detached"] = True
            except Exception as exc:
                errors.append({"stage": "detach", "error": str(exc)})
    result["ok"] = not errors
    return result


def positive(args, name, default):
    value = args.get(name, default)
    if not isinstance(value, int) or isinstance(value, bool) or value < 1:
        raise ValueError(name + " must be a positive integer")
    return value


def capability_inventory(args):
    """Fixed, non-executing local probes; optional package imports may have effects."""
    if args:
        raise ValueError("capability-inventory accepts no arguments")
    names = ("sh", "bash", "python3", "node", "curl-cffi", "jq", "nmap", "nc",
             "ssh", "sshpass", "xxd", "od", "flock", "xvfb-run", "xauth")
    tools = {}
    for name in names:
        path = shutil.which(name)
        tools[name] = {"available": path is not None, "path": path[:512] if path else None,
                       "path_truncated": bool(path and len(path) > 512)}
    modules = {}
    for name in ("pydash", "paramiko", "pexpect", "cloakbrowser", "playwright", "psutil", "curl_cffi"):
        item = {"spec_found": False, "version": None, "import_verified": False}
        try:
            item["spec_found"] = importlib.util.find_spec(name) is not None
        except (ImportError, ValueError, AttributeError):
            item["spec_check"] = "unknown"
        try:
            item["version"] = importlib.metadata.version(name)[:128]
        except importlib.metadata.PackageNotFoundError:
            pass
        modules[name] = item
    binary = os.environ.get("CLOAKBROWSER_BINARY_PATH", "")
    cloak = {"sdk": modules["cloakbrowser"],
               "binary": {"configured": bool(binary), "is_file": None, "executable": None},
               "display": {"configured": bool(os.environ.get("DISPLAY")),
                           "connection_verified": False,
                           "xvfb_run_available": tools["xvfb-run"]["available"],
                           "xauth_available": tools["xauth"]["available"]},
               "launch_verified": False,
               "tool_search_query": "cloakbrowser"}
    if binary:
        cloak["binary"].update(is_file=os.path.isfile(binary), executable=os.access(binary, os.X_OK))
    lightpanda = os.environ.get("LIGHTPANDA_BINARY_PATH") or shutil.which("lightpanda")
    browser = {"preferred": "lightpanda", "fallback": "cloakbrowser",
               "lightpanda": {"binary": {"configured": bool(lightpanda),
                                         "is_file": os.path.isfile(lightpanda) if lightpanda else None,
                                         "executable": os.access(lightpanda, os.X_OK) if lightpanda else None},
                             "protocol": modules["playwright"], "lifecycle": modules["psutil"],
                             "launch_verified": False, "tool_search_query": "lightpanda"},
               "cloakbrowser": cloak}
    alternatives = {"http": [],
                    "hex": ["Python bytes.hex()/bytes.fromhex() (stdlib)"],
                    "ssh_protocol_candidates": [], "ssh_password_candidates": []}
    if modules["curl_cffi"]["spec_found"]:
        alternatives["http"].append("curl_cffi; module import/transport unverified")
    for name in ("xxd", "od"):
        if tools[name]["available"]:
            alternatives["hex"].append(name)
    if tools["ssh"]["available"]:
        alternatives["ssh_protocol_candidates"].append("OpenSSH ssh; authentication/session unverified")
        if tools["sshpass"]["available"]:
            alternatives["ssh_password_candidates"].append("sshpass + ssh; use private FD, not password argv")
        if modules["pexpect"]["spec_found"]:
            alternatives["ssh_password_candidates"].append("pexpect + ssh; module import/session unverified")
    if modules["paramiko"]["spec_found"]:
        alternatives["ssh_protocol_candidates"].append("paramiko; module import/session unverified")
        alternatives["ssh_password_candidates"].append("paramiko; module import/session unverified")
    return {"scope": "local discovery only; no network, optional imports, command execution, installation or browser launch",
            "python_version": sys.version.split()[0], "tools": tools, "modules": modules,
            "browser": browser, "alternatives": alternatives,
            "guidance": [
                "Read once per unchanged environment; do not repeatedly attempt offline pip installs.",
                "PATH/spec/metadata presence is not execution or import verification; verify the selected operation.",
                "TCP port-probe/socket proves connectivity only, not SSH handshake, authentication or command execution.",
                "pydash path semantics require the target version and a small local check, not a hand-written approximation.",
                "For real web pages prefer the browser; use curl_cffi HTTP tools only when browser use is unsuitable. Never replay an uncertain request with another client.",
                "Prefer tool_search query=lightpanda; use cloakbrowser for unavailable/unsupported Lightpanda or actual rendering/screenshots. Select the stable ID and read its schema/Skill.",
                "tool_search matches all query terms; zero matches is not an empty catalog. Retry an exact short term or empty paginated query.",
                "Browser binary/display checks are prerequisites only; unconfigured binary location remains unknown. Never replay an uncertain action in a backup browser."]}


def http_request(args):
    from curl_cffi import CurlECode, requests
    from curl_cffi.curl import CURL_WRITEFUNC_ERROR
    from curl_cffi.requests.exceptions import RequestException

    verify_tls = args.get("verify_tls", True)
    if not isinstance(verify_tls, bool):
        raise ValueError("verify_tls must be a boolean")
    limit = positive(args, "max_bytes", 1048576)
    content = bytearray()

    def receive(chunk):
        content.extend(chunk[:limit + 1 - len(content)])
        # Abort at the sentinel byte instead of buffering the rest of the response.
        return CURL_WRITEFUNC_ERROR if len(content) > limit else len(chunk)

    body = args.get("body")
    method = args.get("method", "GET").upper()
    with requests.Session(retry=0) as session:
        try:
            response = session.request(
                method, args["url"], data=body.encode("utf-8") if body is not None else None,
                headers=args.get("headers", {}), timeout=args.get("timeout", 10),
                verify=verify_tls, impersonate=args.get("impersonate", "chrome"), content_callback=receive,
                allow_redirects=method in ("GET", "HEAD"))
        except RequestException as exc:
            if exc.code != CurlECode.WRITE_ERROR or len(content) <= limit:
                raise
            response = exc.response
        content_type = Message()
        content_type["content-type"] = response.headers.get("content-type", "")
        return {"url": response.url, "status": response.status_code,
                "tls_verified": verify_tls if response.url.startswith("https:") else None,
                "headers": response.headers.multi_items(),
                "body": content[:limit].decode(content_type.get_content_charset() or "utf-8", "replace"),
                "truncated": len(content) > limit}


class Page(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.text, self.title, self.links, self.forms = [], [], [], []
        self.form = None
        self.hidden, self.in_title = 0, False

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag in ("script", "style"):
            self.hidden += 1
        if tag == "title":
            self.in_title = True
        if tag == "a":
            href = attrs.get("href")
            if href is not None:
                self.links.append(href)
        if tag == "form":
            self.form = {"action": attrs.get("action", ""), "method": attrs.get("method", "get").upper(), "fields": []}
            self.forms.append(self.form)
        elif self.form is not None and tag in ("input", "textarea", "select", "button"):
            self.form["fields"].append({"tag": tag, "name": attrs.get("name", ""),
                                        "type": attrs.get("type", "text" if tag == "input" else tag),
                                        "value": attrs.get("value", "")})

    def handle_endtag(self, tag):
        if tag in ("script", "style"):
            self.hidden = max(0, self.hidden - 1)
        if tag == "title":
            self.in_title = False
        if tag == "form":
            self.form = None

    def handle_data(self, data):
        if self.in_title:
            self.title.append(data)
        if not self.hidden and data.strip():
            self.text.append(data.strip())


def page_fetch(args):
    response = http_request(args)
    page = Page()
    page.feed(response.pop("body"))
    for form in page.forms:
        form["action"] = urllib.parse.urljoin(response["url"], form["action"])
    response.update(title=" ".join(page.title), text="\n".join(page.text),
                    links=list(dict.fromkeys(urllib.parse.urljoin(response["url"], link) for link in page.links)), forms=page.forms)
    return response


def dns_resolve(args):
    records = socket.getaddrinfo(args["host"], args.get("port", 0), type=socket.SOCK_STREAM)
    addresses = sorted({(socket.AddressFamily(family).name, address[0]) for family, _, _, _, address in records})
    return {"host": args["host"], "addresses": [{"family": family, "address": address} for family, address in addresses]}


def port_probe(args):
    if not isinstance(args["ports"], list) or any(not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535 for port in args["ports"]):
        raise ValueError("ports must be an array of integers from 1 to 65535")
    results = []
    for port in args["ports"]:
        started = time.monotonic()
        item = {"port": port, "connected": False}
        try:
            with socket.create_connection((args["host"], port), args.get("timeout", 2)) as connection:
                item.update(connected=True, address=connection.getpeername()[0])
        except OSError as exc:
            item["error"] = str(exc)
        item["elapsed_ms"] = round((time.monotonic() - started) * 1000, 3)
        results.append(item)
    return {"host": args["host"], "ports": results}


def binary_info(args):
    path = pathlib.Path(args["path"])
    digest = hashlib.sha256()
    with path.open("rb") as source:
        header = source.read(4096)
        digest.update(header)
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    result = {"path": str(path.resolve()), "size": path.stat().st_size,
              "sha256": digest.hexdigest(), "format": "unknown"}
    if header.startswith(b"\x7fELF"):
        result["format"] = "ELF"
        if len(header) < 20 or header[4] not in (1, 2) or header[5] not in (1, 2):
            raise ValueError("truncated or invalid ELF header")
        endian = "<" if header[5] == 1 else ">"
        kind, machine = struct.unpack_from(endian + "HH", header, 16)
        result.update(bits=32 if header[4] == 1 else 64, endian="little" if endian == "<" else "big",
                      machine=machine, type=kind)
    elif header.startswith(b"MZ"):
        result["format"] = "DOS"
        if len(header) >= 64:
            offset = struct.unpack_from("<I", header, 60)[0]
            with path.open("rb") as source:
                source.seek(offset)
                pe = source.read(26)
            if pe.startswith(b"PE\0\0"):
                if len(pe) < 26:
                    raise ValueError("truncated PE header")
                machine, sections = struct.unpack_from("<HH", pe, 4)
                magic = struct.unpack_from("<H", pe, 24)[0]
                result.update(format="PE", machine=machine, sections=sections,
                              bits={0x10b: 32, 0x20b: 64}.get(magic))
    elif header[:4] in (b"\xce\xfa\xed\xfe", b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xce", b"\xfe\xed\xfa\xcf"):
        endian = "<" if header[0] in (0xce, 0xcf) else ">"
        if len(header) < 16:
            raise ValueError("truncated Mach-O header")
        cpu, subtype, kind = struct.unpack_from(endian + "III", header, 4)
        result.update(format="Mach-O", bits=64 if 0xcf in header[:4] else 32, machine=cpu,
                      subtype=subtype, type=kind, endian="little" if endian == "<" else "big")
    elif header[:4] == b"\xca\xfe\xba\xbe":
        result.update(format="ambiguous", format_candidates=["Java class", "Mach-O universal"])
    elif header[:4] in (b"\xbe\xba\xfe\xca", b"\xca\xfe\xba\xbf", b"\xbf\xba\xfe\xca"):
        result["format"] = "Mach-O universal"
    return result


def strings(args):
    minimum, limit = positive(args, "min_length", 4), positive(args, "limit", 1000)
    encoding = args.get("encoding", "ascii")
    if encoding not in ("ascii", "utf-16le"):
        raise ValueError("encoding must be ascii or utf-16le")
    pattern = rb"[\x20-\x7e]" if encoding == "ascii" else rb"(?:[\x20-\x7e]\x00)"
    matches = re.finditer(pattern + ("{%d,}" % minimum).encode("ascii"), pathlib.Path(args["path"]).read_bytes())
    found, truncated = [], False
    for match in matches:
        if len(found) == limit:
            truncated = True
            break
        found.append({"offset": match.start(), "text": match.group().decode(encoding)})
    return {"path": args["path"], "encoding": encoding, "strings": found, "truncated": truncated}


def analysis(args):
    options = dict(args)
    options.setdefault("limit", 100)
    return {"binary": binary_info(args), "strings": strings(options), "scope": "static metadata and strings only"}


def source_files(args, glob="*"):
    path = pathlib.Path(args["path"])
    if not path.exists():
        raise FileNotFoundError(str(path))
    return [path] if path.is_file() else (entry for entry in path.rglob(args.get("glob", glob)) if entry.is_file())


def code_search(args):
    pattern, limit = args["pattern"], positive(args, "limit", 200)
    expression = re.compile(pattern) if args.get("regex", False) else None
    found, errors = [], []
    for path in source_files(args):
        try:
            data = path.read_bytes()
            if b"\0" in data:
                continue
            for line, text in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
                if (expression.search(text) if expression else pattern in text):
                    if len(found) == limit:
                        return {"matches": found, "errors": errors, "truncated": True}
                    found.append({"path": str(path.resolve()), "line": line, "text": text})
        except OSError as exc:
            errors.append({"path": str(path), "error": str(exc)})
    return {"matches": found, "errors": errors, "truncated": False}


def call_name(node):
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return call_name(node.value) + "." + node.attr
    return ""


def python_ast_audit(args):
    rules = {"eval": "dynamic-code", "exec": "dynamic-code", "os.system": "shell-command",
             "os.popen": "shell-command", "pickle.load": "deserialization", "pickle.loads": "deserialization",
             "marshal.load": "deserialization", "marshal.loads": "deserialization", "yaml.load": "yaml-loader",
             "tempfile.mktemp": "temporary-path-race"}
    findings, errors, limit = [], [], positive(args, "limit", 200)
    for path in source_files(args, "*.py"):
        try:
            # ast.parse accepts bytes and honors Python's source encoding cookie.
            source = path.read_bytes()
            tree = ast.parse(source, filename=str(path))
        except (OSError, SyntaxError) as exc:
            errors.append({"path": str(path), "error": str(exc)})
            continue
        aliases = {}
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    aliases[alias.asname or alias.name.split(".")[0]] = alias.name if alias.asname else alias.name.split(".")[0]
            elif isinstance(node, ast.ImportFrom) and node.module:
                for alias in node.names:
                    aliases[alias.asname or alias.name] = node.module + "." + alias.name
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            name = call_name(node.func)
            base, dot, tail = name.partition(".")
            name = aliases.get(base, base) + dot + tail
            rule = rules.get(name)
            if name.startswith("subprocess.") and any(keyword.arg == "shell" and isinstance(keyword.value, ast.Constant) and keyword.value.value is True for keyword in node.keywords):
                rule = "subprocess-shell"
            if rule:
                if len(findings) == limit:
                    return {"findings": findings, "errors": errors, "truncated": True, "heuristic": True}
                findings.append({"path": str(path.resolve()), "line": node.lineno, "column": node.col_offset,
                                 "call": name, "rule": rule})
    return {"findings": findings, "errors": errors, "truncated": False, "heuristic": True}


def disassemble(args):
    import capstone as cs
    arches = {"x86": (cs.CS_ARCH_X86, cs.CS_MODE_32), "x64": (cs.CS_ARCH_X86, cs.CS_MODE_64),
              "arm": (cs.CS_ARCH_ARM, cs.CS_MODE_ARM), "thumb": (cs.CS_ARCH_ARM, cs.CS_MODE_THUMB),
              "arm64": (cs.CS_ARCH_ARM64, cs.CS_MODE_ARM), "mips": (cs.CS_ARCH_MIPS, cs.CS_MODE_MIPS32),
              "mips64": (cs.CS_ARCH_MIPS, cs.CS_MODE_MIPS64)}
    arch, mode = arches[args["arch"]]
    endian = args.get("endian", "little")
    if endian not in ("little", "big"):
        raise ValueError("endian must be little or big")
    mode |= cs.CS_MODE_BIG_ENDIAN if endian == "big" else cs.CS_MODE_LITTLE_ENDIAN
    size, count = positive(args, "max_bytes", 4096), positive(args, "max_instructions", 256)
    with open(args["path"], "rb") as source:
        source.seek(args.get("offset", 0))
        data = source.read(size)
    engine = cs.Cs(arch, mode)
    instructions = [{"address": item.address, "bytes": item.bytes.hex(), "mnemonic": item.mnemonic, "operands": item.op_str}
                    for item in engine.disasm(data, args.get("address", 0), count=count)]
    return {"arch": args["arch"], "bytes_read": len(data), "decoded_bytes": sum(len(item["bytes"]) // 2 for item in instructions),
            "instructions": instructions}


TOOLS = {"capability-inventory": capability_inventory,
         "http-request": http_request, "page-fetch": page_fetch, "dns-resolve": dns_resolve,
         "port-probe": port_probe, "binary-info": binary_info, "strings": strings, "analysis": analysis,
         "code-search": code_search, "python-ast-audit": python_ast_audit,
         "disassemble": disassemble, "frida": frida_tool}


def main():
    try:
        args = json.load(sys.stdin)
        if not isinstance(args, dict):
            raise ValueError("arguments must be a JSON object")
        result = TOOLS[sys.argv[1]](args)
        print(json.dumps(result, ensure_ascii=True), flush=True)
        return 0 if result.get("ok", True) else 1
    except Exception as exc:
        print(json.dumps({"error": {"type": type(exc).__name__, "message": str(exc)}}), flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
