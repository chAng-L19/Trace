#!/usr/bin/env python3
"""Server-only offline capability acceptance in one disposable container.

python3 docker/capability_runtime_test.py --image trace:tsecbench-hosted-20261103 \
  --source /tmp/trace-p0/source --bundle /tmp/trace-p0/frozen --report /tmp/capabilities.json
No image build, official service access, platform request, or host account change.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import uuid


def run(argv, **kwargs):
    return subprocess.run(argv, check=True, capture_output=True, text=True,
                          timeout=kwargs.pop("timeout", 30), **kwargs)


def browser_check():
    import cloakbrowser
    import psutil
    from urllib.parse import quote
    assert cloakbrowser.__version__ == "0.6.4"
    assert os.environ.get("CLOAKBROWSER_AUTO_UPDATE") == "false"
    binary = Path(os.environ["CLOAKBROWSER_BINARY_PATH"]).resolve()
    assert binary.is_file() and os.access(binary, os.X_OK)
    font = Path("/usr/local/share/fonts/trace-noto-cjk/NotoSansCJK-Regular.ttc")
    license_path = Path("/opt/trace/licenses/noto-cjk/LICENSE.txt")
    expected = dict((line.split()[1], line.split()[0]) for line in Path("/source/docker/capabilities.sha256").read_text().splitlines() if line.strip())
    assert hashlib.sha256(font.read_bytes()).hexdigest() == expected[font.name]
    assert hashlib.sha256(license_path.read_bytes()).hexdigest() == expected["NotoSansCJK-LICENSE.txt"]
    browser = cloakbrowser.launch(headless=False, args=["--no-sandbox"], handle_sigterm=False, handle_sigint=False)
    try:
        page = browser.new_page()
        html = """<!doctype html><title>trace-capability-offline</title>
<input id=value><button id=verify onclick="document.querySelector('#status').textContent=document.querySelector('#value').value">Verify</button><p id=status>ready</p>"""
        page.goto("data:text/html," + quote(html), timeout=30000)
        page.locator("#value").fill("capability-clicked")
        page.locator("#verify").click(timeout=5000)
        assert page.locator("#status").inner_text() == "capability-clicked"
        assert page.title() == "trace-capability-offline" and page.evaluate("1+1") == 2
        page.evaluate("() => { const p=document.createElement('p'); p.id='cjk'; p.style.fontFamily='Noto Sans CJK SC'; p.textContent='中文字体验证'; document.body.appendChild(p); }")
        assert page.locator("#cjk").inner_text() == "中文字体验证"
        # Observe the actual platform font used for Chinese glyphs, not only a
        # CSS family declaration or document.fonts.check's fallback behavior.
        cdp = page.context.new_cdp_session(page)
        try:
            cdp.send("DOM.enable")
            cdp.send("CSS.enable")
            root = cdp.send("DOM.getDocument")["root"]["nodeId"]
            node = cdp.send("DOM.querySelector", {"nodeId": root, "selector": "#cjk"})["nodeId"]
            fonts = cdp.send("CSS.getPlatformFontsForNode", {"nodeId": node})["fonts"]
            assert any("Noto Sans CJK" in item["familyName"] and item["glyphCount"] > 0 for item in fonts), fonts
        finally:
            cdp.detach()
        png = page.screenshot(type="png", timeout=10000)
        assert png[:8] == b"\x89PNG\r\n\x1a\n" and png[12:16] == b"IHDR"
        commands = [p.cmdline() for p in psutil.Process().children(recursive=True)
                    if p.is_running() and p.exe() == str(binary)]
        assert commands and all(not any(a.startswith("--headless") for a in c) for c in commands)
        result = {"sdk": cloakbrowser.__version__, "headed_fixed_binary": True,
                  "dom_fill_click": True, "cjk_platform_font": fonts,
                  "font_sha256": expected[font.name], "font_license_sha256": expected["NotoSansCJK-LICENSE.txt"],
                  "png_sha256": hashlib.sha256(png).hexdigest()}
    finally:
        browser.close()
    print(json.dumps(result))


def ssh_check(sshd, root):
    assert Path(sshd).is_file(), "fixture sshd missing"
    root.chmod(0o755)  # The product's non-root client reads only public known_hosts.
    user = "trace_ssh_fixture"
    password = secrets.token_urlsafe(24)
    run(["useradd", "--no-create-home", "--shell", "/bin/sh", user])
    run(["chpasswd"], input=user + ":" + password + "\n")
    Path("/run/sshd").mkdir(mode=0o755, parents=True, exist_ok=True)
    hostkey, wrongkey = root / "hostkey", root / "wrongkey"
    for path in (hostkey, wrongkey):
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(path)])
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    config = root / "sshd_config"
    config.write_text("\n".join([
        "ListenAddress 127.0.0.1", "Port " + str(port), "HostKey " + str(hostkey),
        "PidFile " + str(root / "sshd.pid"), "UsePAM no", "PasswordAuthentication yes",
        "KbdInteractiveAuthentication no", "AuthenticationMethods password", "PermitRootLogin no",
        "PubkeyAuthentication no", "AllowUsers " + user, "LogLevel ERROR", "UseDNS no"]) + "\n")
    known, wrongknown = root / "known_hosts", root / "wrong_known_hosts"
    for output, key in ((known, hostkey), (wrongknown, wrongkey)):
        public = Path(str(key) + ".pub").read_text().split()
        output.write_text("[127.0.0.1]:" + str(port) + " " + " ".join(public[:2]) + "\n")
    server = subprocess.Popen([sshd, "-D", "-e", "-f", str(config)],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(50):
            assert server.poll() is None, "fixture sshd exited"
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise TimeoutError("fixture sshd readiness")

        def client(secret, hosts):
            readfd, writefd = os.pipe()
            try:
                os.write(writefd, (secret + "\n").encode())
                os.close(writefd)
                writefd = -1
                return subprocess.run([
                    "sshpass", "-d", str(readfd), "ssh", "-F", "/dev/null", "-p", str(port),
                    "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + str(hosts),
                    "-o", "GlobalKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5",
                    "-o", "ConnectionAttempts=1", "-o", "NumberOfPasswordPrompts=1",
                    "-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no",
                    user + "@127.0.0.1", "printf 'trace-ssh-protocol-ok\\n'"],
                    pass_fds=(readfd,), capture_output=True, text=True, timeout=15, user="trace", group="trace")
            finally:
                os.close(readfd)
                if writefd != -1:
                    os.close(writefd)

        good = client(password, known)
        if good.returncode != 0:
            raise AssertionError("SSH positive control code=" + str(good.returncode) + " permission=" + str("Permission denied" in good.stderr) + " hostkey=" + str("Host key verification failed" in good.stderr) + " known_hosts_read=" + str("load_hostkeys" in good.stderr))
        assert good.returncode == 0 and good.stdout == "trace-ssh-protocol-ok\n", "SSH positive control"
        bad_password = client("wrong-" + password, known)
        assert bad_password.returncode != 0 and not bad_password.stdout, "SSH wrong-password control"
        assert "Permission denied" in bad_password.stderr, "password rejection not observed"
        bad_key = client(password, wrongknown)
        assert bad_key.returncode != 0 and not bad_key.stdout, "SSH wrong-hostkey control"
        assert "REMOTE HOST IDENTIFICATION HAS CHANGED" in bad_key.stderr, "hostkey rejection not observed"
        return {"handshake_hostkey_auth_command": True, "password_via_private_fd": True,
                "wrong_password_rejected": True, "wrong_hostkey_rejected": True}
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()
            server.wait(timeout=5)
        run(["userdel", user])


def inside(args):
    assert os.geteuid() == 0 and args.source == "/source" and args.bundle == "/frozen"
    stage = "offline_install"
    try:
        # Hash/missing failures must happen before any package transaction.
        stage = "bundle_negative_controls"
        with tempfile.TemporaryDirectory(prefix="trace-bundle-negative-") as tmp:
            broken = Path(tmp)
            names = [line.split()[1] for line in Path("/source/docker/capabilities.sha256").read_text().splitlines() if line.strip()]
            for name in names:
                shutil.copyfile(Path("/frozen") / name, broken / name)
            before = run(["dpkg-query", "-W"]).stdout
            wheel = broken / "pydash-8.0.5-py3-none-any.whl"
            wheel.write_bytes(wheel.read_bytes() + b"corrupt-fixture")
            command = ["sh", "/source/docker/install-capabilities.sh", str(broken), "/source/docker/capabilities.sha256"]
            assert subprocess.run(command, capture_output=True, timeout=20).returncode != 0
            wheel.unlink()
            assert subprocess.run(command, capture_output=True, timeout=20).returncode != 0
            shutil.copyfile(Path("/frozen") / wheel.name, wheel)
            installed_font = Path("/usr/local/share/fonts/trace-noto-cjk/NotoSansCJK-Regular.ttc")
            installed_license = Path("/opt/trace/licenses/noto-cjk/LICENSE.txt")
            installed_before = [path.read_bytes() if path.exists() else None for path in (installed_font, installed_license)]
            for name in ("NotoSansCJK-Regular.ttc", "NotoSansCJK-LICENSE.txt"):
                target = broken / name
                target.write_bytes(target.read_bytes() + b"corrupt-font-fixture")
                assert subprocess.run(command, capture_output=True, timeout=20).returncode != 0
                target.unlink()
                assert subprocess.run(command, capture_output=True, timeout=20).returncode != 0
                shutil.copyfile(Path("/frozen") / name, target)
                assert installed_before == [path.read_bytes() if path.exists() else None for path in (installed_font, installed_license)], "invalid bundle changed installed font or license"
            assert before == run(["dpkg-query", "-W"]).stdout, "invalid bundle changed package state"
        stage = "offline_install"
        # A fixture package must never start/replace a service during dpkg hooks.
        policy = Path("/usr/sbin/policy-rc.d")
        old_policy = policy.read_bytes() if policy.exists() else None
        old_mode = policy.stat().st_mode & 0o777 if policy.exists() else None
        policy.write_text("#!/bin/sh\nexit 101\n")
        policy.chmod(0o755)
        try:
            run(["sh", "/source/docker/install-capabilities.sh", "/frozen",
                 "/source/docker/capabilities.sha256"], timeout=120)
            debs = sorted(Path("/frozen/sshd-fixture").glob("*.deb"))
            if debs:
                manifest = "/frozen/sshd-fixture.sha256"
                assert Path(manifest).is_file(), "fixture deb hashes missing"
                run(["sha256sum", "-c", manifest], cwd="/frozen")
                run(["dpkg", "-i", *map(str, debs)], timeout=120)
        finally:
            if old_policy is None:
                policy.unlink()
            else:
                policy.write_bytes(old_policy)
                policy.chmod(old_mode)
        stage = "pydash"
        import importlib.metadata
        import pydash
        assert importlib.metadata.version("pydash") == "8.0.5"
        value = {}
        pydash.set_(value, "items[0].name", "fixture")
        assert value == {"items": [{"name": "fixture"}]}
        assert bytes.fromhex(b"trace-hex".hex()) == b"trace-hex"
        run([sys.executable, "-c", "import importlib.metadata,pydash; assert importlib.metadata.version('pydash')=='8.0.5'; d={}; pydash.set_(d,'a.b',7); assert d=={'a':{'b':7}}"], user="trace", group="trace")
        stage = "inventory"
        inventory = json.loads(run([sys.executable, "/source/internal/localtool/helper.py",
                                   "capability-inventory"], input="{}\n").stdout)
        assert inventory["tools"]["ssh"]["available"] and inventory["tools"]["sshpass"]["available"]
        assert inventory["modules"]["pydash"]["version"] == "8.0.5"
        assert not inventory["browser"]["launch_verified"]
        stage = "ssh"
        with tempfile.TemporaryDirectory(prefix="trace-ssh-capability-") as tmp:
            ssh = ssh_check(args.sshd_path, Path(tmp))
        stage = "browser"
        product_env = dict(os.environ, HOME="/home/trace", USER="trace", LOGNAME="trace")
        browser = json.loads(run(["xvfb-run", "-a", sys.executable, __file__, "--browser"], timeout=100, user="trace", group="trace", env=product_env).stdout.strip().splitlines()[-1])
        result = {"status": "pass", "offline_install": True, "pydash_version": "8.0.5",
                  "hash_mismatch_rejected": True, "missing_bundle_rejected": True,
                  "product_client_uid": 1000,
                  "pydash_set_path": True, "stdlib_hex_roundtrip": True,
                  "inventory": True, "ssh": ssh, "browser": browser}
    except Exception as error:
        result = {"status": "fail", "stage": stage, "error_type": type(error).__name__}
        if isinstance(error, AssertionError):
            result["assertion"] = str(error)[:400]
    print(json.dumps(result))
    return 0 if result["status"] == "pass" else 1


def main(args):
    source, bundle = Path(args.source).resolve(), Path(args.bundle).resolve()
    assert (source / "docker/capability_runtime_test.py").is_file() and bundle.is_dir()
    name = "trace-capability-" + uuid.uuid4().hex[:12]
    report = {"status": "running", "image": args.image, "container": name, "network": "none",
              "scope": "Disposable acceptance only; no official service, platform or image build"}
    try:
        completed = subprocess.run([
            "docker", "run", "--rm", "--name", name, "--network", "none", "--user", "0",
            "--mount", "type=bind,src=" + str(source) + ",dst=/source,readonly",
            "--mount", "type=bind,src=" + str(bundle) + ",dst=/frozen,readonly",
            "--env", "CLOAKBROWSER_AUTO_UPDATE=false", "--entrypoint", "python3", args.image,
            "/source/docker/capability_runtime_test.py", "--inside", "--source", "/source",
            "--bundle", "/frozen", "--sshd-path", args.sshd_path],
            capture_output=True, text=True, timeout=360)
        report["docker_exit_code"] = completed.returncode
        lines = completed.stdout.strip().splitlines()
        if not lines:
            raise RuntimeError("docker returned no fixture report; check Docker access and bind paths")
        result = json.loads(lines[-1])
        report.update(result)
        if completed.returncode:
            report["status"] = "fail"
    except Exception as error:
        report.update(status="fail", error_type=type(error).__name__)
    finally:
        subprocess.run(["docker", "rm", "--force", name], capture_output=True, timeout=30)
        check = subprocess.run(["docker", "container", "ls", "--all", "--quiet", "--filter", "name=^/" + name + "$"],
                               capture_output=True, text=True, timeout=15)
        report["container_removed"] = check.returncode == 0 and not check.stdout.strip()
        if not report["container_removed"]:
            report["status"] = "fail"
        Path(args.report).write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(json.dumps(report))
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="trace:tsecbench-hosted-20261103")
    parser.add_argument("--source", default="/source")
    parser.add_argument("--bundle", default="/frozen")
    parser.add_argument("--sshd-path", default="/usr/sbin/sshd")
    parser.add_argument("--report")
    parser.add_argument("--inside", action="store_true")
    parser.add_argument("--browser", action="store_true")
    options = parser.parse_args()
    if options.browser:
        browser_check()
    elif options.inside:
        sys.exit(inside(options))
    else:
        assert options.report, "--report is required"
        sys.exit(main(options))
