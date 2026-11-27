#!/usr/bin/env python3
"""Server-only browser migration parity and offline startup checks; installs nothing.
python3 docker/image_test.py --base trace:frontend-docker-20261029 --image trace:cloakbrowser-20261030
Run p2_runtime_test.py separately for real tini/exec/MCP/SIGINT lifecycle coverage.
"""
import argparse
from collections import Counter
import json
from pathlib import Path
import subprocess
import sys
import uuid

cleanup_records = []
BROWSER_ROOTS = (
    "/opt/trace/runtime/chromium",
    "/opt/trace/runtime/playwright",
    "/opt/trace/runtime/cloakbrowser",
    "/opt/trace/tools/web-analysis",
    "/opt/trace/.trace/skills/drissionpage",
    "/opt/trace/.trace/skills/playwright-cli",
    "/opt/trace/.trace/skills/cloakbrowser",
)


def browser_migration_root(path):
    for root in BROWSER_ROOTS:
        if path == root or path.startswith(root + "/"):
            return root
    prefix = "/opt/trace/runtime/site-packages/"
    if path.startswith(prefix):
        name = path[len(prefix):].split("/", 1)[0]
        if name == "DrissionPage" or (name.lower().startswith("drissionpage-")
                                     and name.endswith(".dist-info")):
            return prefix + name
    return None


def docker(*args, input=None, timeout=60):
    return subprocess.run(["docker", *args], input=input, text=True,
                          capture_output=True, check=True, timeout=timeout).stdout


def container(image, *command, entrypoint=None, user=None, timeout=60):
    name = "trace-image-test-" + uuid.uuid4().hex[:12]
    args = ["run", "--rm", "--name", name, "--network", "none"]
    if entrypoint:
        args += ["--entrypoint", entrypoint]
    if user:
        args += ["--user", user]
    record = {"container": name, "removed": False}
    cleanup_records.append(record)
    try:
        return docker(*args, image, *command, timeout=timeout)
    finally:
        # A CLI timeout does not stop its container. Remove only this invocation.
        subprocess.run(["docker", "rm", "--force", name], capture_output=True, timeout=30)
        remaining = docker("container", "ls", "--all", "--quiet", "--filter", "name=^/" + name + "$")
        record["removed"] = not remaining.strip()
        if not record["removed"]:
            raise RuntimeError("fixture container cleanup failed: " + name)


def snapshot(image):
    # Compare rootfs inventory/modes/owners/xattrs, not historical content hashes.
    code = r'''
import json, os, stat, subprocess
excluded = {"/proc", "/sys", "/dev", "/.dockerenv", "/etc/hosts", "/etc/hostname", "/etc/resolv.conf"}
updated = {
    "/usr/local/bin/trace", "/opt/trace/prompts/agent.md", "/opt/trace/prompts/coordinator.md",
    "/opt/trace/prompts/worker.md", "/opt/trace/prompts/recon.md",
    "/opt/trace/examples/tsecbench-task.md", "/opt/trace/docker/entrypoint.sh",
}
files = {}
for directory, names, entries in os.walk("/"):
    names[:] = [name for name in names if os.path.join(directory, name) not in excluded]
    for name in [".", *names, *entries]:
        path = os.path.normpath(os.path.join(directory, name))
        if path in excluded or path in updated or path == "/opt/trace/licenses" or path.startswith("/opt/trace/licenses/"):
            continue
        info = os.lstat(path)
        attrs = {key: os.getxattr(path, key, follow_symlinks=False).hex()
                 for key in os.listxattr(path, follow_symlinks=False)}
        files[path] = [info.st_mode, info.st_uid, info.st_gid,
                       info.st_size if stat.S_ISREG(info.st_mode) else 0,
                       os.readlink(path) if stat.S_ISLNK(info.st_mode) else "", attrs]
anchors = {}
for path in ("/usr/bin/tini", "/bin/bash", "/lib64/ld-linux-x86-64.so.2",
             "/etc/passwd", "/etc/group", "/etc/ssl/certs/ca-certificates.crt",
             "/home/trace", "/state", "/workspace"):
    info = os.stat(path)
    anchors[path] = [info.st_mode, info.st_uid, info.st_gid]
for path in ("/usr/local/bin/trace", "/opt/trace/docker/entrypoint.sh"):
    info = os.stat(path)
    assert info.st_uid == info.st_gid == 0 and os.access(path, os.X_OK), path
assert not os.listdir("/state") and not os.listdir("/workspace")
packages = subprocess.check_output(["dpkg-query", "-W", "-f=${Package}=${Version}\n"], text=True).splitlines()
print(json.dumps({"files": files, "anchors": anchors, "packages": sorted(packages)}))
'''
    return json.loads(container(image, "-c", code, entrypoint="python3", user="0"))


def run(base, image):
    if sys.platform != "linux":
        raise RuntimeError("run on the designated Linux server")
    old, new = [json.loads(docker("image", "inspect", tag))[0] for tag in (base, image)]
    fields = ("User", "WorkingDir", "Entrypoint", "Cmd", "StopSignal", "Labels",
              "Volumes", "ExposedPorts", "Healthcheck", "Shell", "OnBuild",
              "AttachStdin", "AttachStdout", "AttachStderr", "OpenStdin", "StdinOnce", "Tty")
    for field in fields:
        assert old["Config"].get(field) == new["Config"].get(field), field
    for field in ("Architecture", "Os"):
        assert old[field] == new[field], field
    # Only browser settings and the removed pi host setting may change.
    before_env = dict(item.split("=", 1) for item in old["Config"]["Env"])
    after_env = dict(item.split("=", 1) for item in new["Config"]["Env"])
    expected_env = dict(before_env)
    for key in ("TRACE_BROWSER_EXECUTABLE", "PLAYWRIGHT_MCP_EXECUTABLE_PATH",
                "PLAYWRIGHT_MCP_SANDBOX", "REA_BROWSER_EXECUTABLE"):
        expected_env.pop(key, None)
    expected_env["PATH"] = ":".join(value for value in before_env["PATH"].split(":")
                                    if value != "/opt/trace/runtime/playwright/node_modules/.bin")
    expected_env["PYTHONPATH"] = "/opt/trace/runtime/cloakbrowser/site-packages:" + before_env["PYTHONPATH"]
    expected_env["CLOAKBROWSER_BINARY_PATH"] = "/opt/trace/runtime/cloakbrowser/binary/chrome"
    expected_env["CLOAKBROWSER_AUTO_UPDATE"] = "false"
    expected_env["TRACE_PI_RUNTIME"] = ""
    assert expected_env == after_env, "unexpected Env migration"
    assert new["Config"]["User"] == "trace"
    assert new["Config"]["StopSignal"] == "SIGINT"
    assert new["Config"]["Entrypoint"] == ["/usr/bin/tini", "-g", "--", "/opt/trace/docker/entrypoint.sh"]
    before, after = snapshot(base), snapshot(image)
    changed = sorted(p for p in before["files"].keys() | after["files"].keys()
                     if before["files"].get(p) != after["files"].get(p))
    removed_pi = {p for p in changed
                  if (p == "/opt/trace/runtime/pi" or p.startswith("/opt/trace/runtime/pi/"))
                  and p not in after["files"]}
    unexpected = [p for p in changed
                  if browser_migration_root(p) is None and p not in removed_pi]
    assert not unexpected, "non-browser frozen inventory changed: " + str(unexpected[:10])
    migration_counts = Counter(browser_migration_root(p) for p in changed if p not in removed_pi)
    assert migration_counts or removed_pi, "runtime migration inventory unchanged"
    for root in (*BROWSER_ROOTS[:2], *BROWSER_ROOTS[4:6]):
        assert not any(p == root or p.startswith(root + "/") for p in after["files"]), root
    assert not any(p.startswith("/opt/trace/runtime/site-packages/")
                   and browser_migration_root(p) is not None for p in after["files"]), "DrissionPage metadata remains"
    for field in ("anchors", "packages"):
        if before[field] != after[field]:
            raise AssertionError("frozen " + field + " changed")
    smoke = r'''set -euo pipefail
test "$(id -u)" = 1000
test "$PWD" = /workspace
test -w /state
test -w /workspace
python3 -c 'import capstone, droidasc, frida, cloakbrowser, playwright, importlib.util, sys; assert sys.version_info[:2] == (3, 12); assert cloakbrowser.__version__ == "0.6.4"; assert importlib.util.find_spec("DrissionPage") is None; print("Python 3.12 tools / CloakBrowser 0.6.4: PASS")'
if command -v playwright-cli >/dev/null 2>&1; then echo 'old Playwright CLI remains' >&2; exit 1; fi
test ! -e /opt/trace/runtime/chromium
test ! -e /opt/trace/runtime/playwright
test ! -e /opt/trace/runtime/pi
test -z "${TRACE_PI_RUNTIME:-}"
test -x "$CLOAKBROWSER_BINARY_PATH"
test "$CLOAKBROWSER_AUTO_UPDATE" = false
test -s /opt/trace/.trace/skills/cloakbrowser/SKILL.md
test ! -e /opt/trace/.trace/skills/drissionpage
test ! -e /opt/trace/.trace/skills/playwright-cli
node /opt/trace/tools/web-analysis/dist/trace-cli.js --catalog | python3 -c 'import json,sys; x=json.load(sys.stdin); assert len(x["operations"]) == 28; print("web-analysis operations:", len(x["operations"]))'
xvfb-run -a python3 - <<'PY'
import os
from pathlib import Path
from urllib.parse import quote
from cloakbrowser import launch
binary = Path(os.environ["CLOAKBROWSER_BINARY_PATH"]).resolve()
browser = launch(headless=False, args=["--no-sandbox"])
try:
    page = browser.new_page()
    # Upstream tests/test_humanize_unit.py:645 uses navigation because
    # set_content can stall; retain that separate compatibility failure.
    html = """<!doctype html><title>trace-python-cloak-offline</title>
<button id=verify onclick="document.querySelector('#status').textContent='clicked'">Verify</button>
<p id=status>ready</p>"""
    page.goto("data:text/html," + quote(html), wait_until="load", timeout=30000)
    assert page.title() == "trace-python-cloak-offline"
    assert page.evaluate("1+1") == 2
    assert page.locator("#status").inner_text() == "ready"
    page.locator("#verify").click(timeout=5000)
    assert page.locator("#status").inner_text() == "clicked"
    png = page.screenshot(type="png", timeout=10000)
    assert len(png) > 24 and png[:8] == b"\x89PNG\r\n\x1a\n" and png[12:16] == b"IHDR"
    assert int.from_bytes(png[16:20], "big") > 0 and int.from_bytes(png[20:24], "big") > 0
    commands = []
    for proc in Path("/proc").glob("[0-9]*"):
        try:
            args = (proc / "cmdline").read_bytes().split(b"\0")
            if args[0] and Path(os.fsdecode(args[0])).resolve() == binary:
                commands.append([os.fsdecode(arg) for arg in args if arg])
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            pass
    assert commands, "fixed CloakBrowser process not found"
    assert not any(arg.startswith("--headless") for args in commands for arg in args)
    assert os.environ.get("DISPLAY"), "Xvfb display missing"
    print("CloakBrowser Python offline navigation/JS/DOM click/PNG/headed/Xvfb/no --headless: PASS")
finally:
    browser.close()
os.environ["CLOAKBROWSER_BINARY_PATH"] = "/nonexistent/trace-cloakbrowser/chrome"
try:
    launch(headless=False)
except FileNotFoundError:
    print("CloakBrowser Python missing binary fails without fallback: PASS")
else:
    raise AssertionError("missing binary launched")
PY
cd /opt/trace/tools/web-analysis
xvfb-run -a node --input-type=module - <<'JS'
import assert from "node:assert/strict";
import {readFileSync, readdirSync, realpathSync} from "node:fs";
import {launch} from "cloakbrowser";
assert.equal(JSON.parse(readFileSync("node_modules/cloakbrowser/package.json")).version, "0.6.4");
const binary = realpathSync(process.env.CLOAKBROWSER_BINARY_PATH);
const browser = await launch({headless:false, args:["--no-sandbox"]});
try {
  const page = await browser.newPage();
  // Upstream js/tests/stealth.test.ts:903 uses navigation because setContent
  // can stall; this does not claim that separate compatibility failure is fixed.
  const html = `<!doctype html><title>trace-js-cloak-offline</title>
<button id=verify onclick="document.querySelector('#status').textContent='clicked'">Verify</button>
<p id=status>ready</p>`;
  await page.goto("data:text/html," + encodeURIComponent(html), {waitUntil:"load", timeout:30000});
  assert.equal(await page.title(), "trace-js-cloak-offline");
  assert.equal(await page.evaluate("1+1"), 2);
  assert.equal(await page.locator("#status").innerText(), "ready");
  await page.locator("#verify").click({timeout:5000});
  assert.equal(await page.locator("#status").innerText(), "clicked");
  const png = await page.screenshot({type:"png", timeout:10000});
  assert.ok(png.length > 24);
  assert.deepEqual(png.subarray(0, 8), Buffer.from([137,80,78,71,13,10,26,10]));
  assert.equal(png.toString("ascii", 12, 16), "IHDR");
  assert.ok(png.readUInt32BE(16) > 0 && png.readUInt32BE(20) > 0);
  const commands = [];
  for (const pid of readdirSync("/proc").filter(p => /^\d+$/.test(p))) {
    try {
      const args = readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").filter(Boolean);
      if (args[0] && realpathSync(args[0]) === binary) commands.push(args);
    } catch (error) {
      if (!["ENOENT", "EACCES", "ESRCH"].includes(error.code)) throw error;
    }
  }
  assert.ok(commands.length, "fixed CloakBrowser process not found");
  assert.ok(!commands.flat().some(arg => arg.startsWith("--headless")));
  assert.ok(process.env.DISPLAY);
  console.log("CloakBrowser JS 0.6.4 offline navigation/JS/DOM click/PNG/headed/Xvfb/no --headless: PASS");
} finally { await browser.close(); }
process.env.CLOAKBROWSER_BINARY_PATH = "/nonexistent/trace-cloakbrowser/chrome";
await assert.rejects(() => launch({headless:false}), error =>
  error.message.includes("/nonexistent/trace-cloakbrowser/chrome") &&
  /does(?: not|n't) exist/.test(error.message));
console.log("CloakBrowser JS missing binary fails without fallback: PASS");
JS
'''
    output = container(image, "-c", smoke, entrypoint="bash")
    version = container(image, "version").strip()
    assert version, "empty trace version"
    assert len(new["RootFS"]["Layers"]) == 1, "expected one merged rootfs layer"
    return {"status": "pass", "base": base, "image": image,
            "non_browser_metadata_equal": True, "non_browser_inventory_equal": True,
            "browser_migration_changed_paths": dict(sorted(migration_counts.items())),
            "environment_changes": {key: {"before": before_env.get(key), "after": after_env.get(key)}
                                    for key in sorted(before_env.keys() | after_env.keys())
                                    if before_env.get(key) != after_env.get(key)},
            "parity_scope": "path/type/mode/owner/size/symlink/xattrs, not file content hashes; browser runtime/skills and rebuilt web-analysis explicitly replaced",
            "inspected_files": len(after["files"]), "system_packages": len(after["packages"]),
            "anchors": after["anchors"], "trace_version": version, "smoke": output.splitlines(),
            "base_layers": len(old["RootFS"]["Layers"]), "layers": len(new["RootFS"]["Layers"]),
            "base_bytes": old["Size"], "bytes": new["Size"]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--report")
    args = parser.parse_args()
    try:
        result = run(args.base, args.image)
    except Exception as error:
        result = {"status": "failed", "base": args.base, "image": args.image,
                  "error": str(error)}
        if isinstance(error, (subprocess.CalledProcessError, subprocess.TimeoutExpired)):
            # Preserve the last successful smoke step as well as the failure.
            # TimeoutExpired may retain bytes even when subprocess text=True.
            for stream in ("stdout", "stderr"):
                value = getattr(error, stream, None)
                result[stream] = value.decode("utf-8", errors="replace") if isinstance(value, bytes) else value
    result["cleanup"] = cleanup_records
    output = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    if args.report:
        Path(args.report).write_text(output, encoding="utf-8")
    print(output, end="")
    sys.exit(0 if result["status"] == "pass" else 1)
