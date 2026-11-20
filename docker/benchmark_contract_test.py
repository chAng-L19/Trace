#!/usr/bin/env python3
"""Server-only stdlib fixture tests for the shipped one-request helper."""
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest

HELPER = Path(__file__).resolve().parents[1] / "examples/tsecbench_api.py"
TOKEN = "synthetic-benchmark-secret"


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        self.respond()

    def do_POST(self):
        self.respond()

    def respond(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.server.calls.append((self.command, self.path, body, self.headers.get("BENCHMARK_TOKEN")))
        status, payload = self.server.reply
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        if status == 302:
            self.send_header("Location", "/redirect-target")
        self.end_headers()
        self.wfile.write(payload)


class BenchmarkContract(unittest.TestCase):
    def test_frozen_build_inputs_and_helper_are_not_omitted(self):
        root = HELPER.parents[1]
        allow = (root / ".dockerignore").read_text()
        for path in ("examples/tsecbench_api.py", "docker/install-capabilities.sh", "docker/capabilities.sha256", "docker/lightpanda.sha256", "scripts/", "scripts/http-requirements.txt"):
            self.assertIn("!" + path, allow)
        for name in ("Dockerfile", "Dockerfile.release", "Dockerfile.hosted"):
            source = (root / "docker" / name).read_text()
            self.assertIn("COPY examples/tsecbench_api.py /opt/trace/examples/tsecbench_api.py", source)
            self.assertIn("LIGHTPANDA_BINARY_PATH=/opt/trace/runtime/lightpanda/lightpanda", source)
            self.assertIn("test -x", source)
            self.assertIn("COPY docker/lightpanda.sha256 /opt/trace/runtime/lightpanda/SHA256SUMS", source)
            self.assertIn("sha256sum -c SHA256SUMS", source)
            self.assertIn("COPY runtime/capabilities/ /tmp/trace-capabilities/", source)
            self.assertIn("COPY docker/install-capabilities.sh docker/capabilities.sha256 scripts/http-requirements.txt /tmp/trace-capability-build/", source)
            self.assertIn("install-capabilities.sh /tmp/trace-capabilities /tmp/trace-capability-build/capabilities.sha256", source)
        installer = (root / "docker/install-capabilities.sh").read_text()
        self.assertLess(installer.index("sha256sum -c"), installer.index("dpkg -i"))
        self.assertIn("--no-index --no-deps", installer)
        lines = (root / "docker/capabilities.sha256").read_text().splitlines()
        self.assertEqual(len(lines), 16)
        self.assertIn('--no-index --require-hashes --find-links "$bundle" --no-compile --upgrade', installer)
        self.assertIn('--target /opt/trace/runtime/site-packages -r "$(dirname "$lock")/http-requirements.txt"', installer)
        for command in ("curl", "curl-cffi"):
            self.assertIn(f"install -m 0755 /opt/trace/runtime/site-packages/bin/curl-cffi /usr/local/bin/{command}\n", installer)
        locked = {name: digest for digest, name in (line.split() for line in lines)}
        self.assertEqual(len(locked), 16)
        requirements = [line for line in (root / "scripts/http-requirements.txt").read_text().splitlines()
                        if line.strip() and not line.startswith("#")]
        self.assertEqual(len(requirements), 8)
        self.assertTrue(any(line.startswith("curl_cffi[cli]==0.17.0 --hash=sha256:") for line in requirements))
        pins = {}
        for requirement in requirements:
            pin, digest = requirement.split(" --hash=sha256:")
            package, version = pin.split("==")
            package = package.split("[")[0].replace("-", "_").lower()
            pins[package] = version
            self.assertRegex(digest, r"^[0-9a-f]{64}$")
            wheels = [name for name in locked if name.endswith(".whl") and name.lower().startswith(package + "-" + version + "-")]
            self.assertEqual(len(wheels), 1, requirement)
            self.assertEqual(locked[wheels[0]], digest, requirement)
        self.assertEqual(pins, {
            "certifi": "2026.10.23", "cffi": "2.1.1", "curl_cffi": "0.17.0",
            "markdown_it_py": "4.3.0", "mdurl": "0.1.2", "pycparser": "3.12",
            "pygments": "2.21.0", "rich": "15.0.0",
        })
        self.assertTrue(any(line.endswith("  NotoSansCJK-Regular.ttc") for line in lines))
        self.assertTrue(any(line.endswith("  NotoSansCJK-LICENSE.txt") for line in lines))
        self.assertLess(installer.index("sha256sum -c"), installer.index("install -m 0644"))
        self.assertIn("/usr/local/share/fonts/trace-noto-cjk", installer)
        self.assertIn("/opt/trace/licenses/noto-cjk", installer)
        hosted = (root / "docker/Dockerfile.hosted").read_text()
        for source, destination in (
            ("tools/web-analysis/", "/opt/trace/tools/web-analysis/"),
            (".trace/skills/asc/", "/opt/trace/.trace/skills/asc/"),
            (".trace/skills/cloakbrowser/", "/opt/trace/.trace/skills/cloakbrowser/"),
            (".trace/skills/lightpanda/", "/opt/trace/.trace/skills/lightpanda/"),
            ("runtime/lightpanda/", "/opt/trace/runtime/lightpanda/"),
            (".trace/skills/frida/", "/opt/trace/.trace/skills/frida/"),
            (".trace/skills/web-analysis/", "/opt/trace/.trace/skills/web-analysis/"),
            ("licenses/", "/opt/trace/licenses/"),
        ):
            self.assertIn(f"COPY {source} {destination}", hosted)
        self.assertIn("RUN rm -rf /opt/trace/tools/web-analysis", hosted)
        removal = hosted.split("COPY tools/web-analysis/", 1)[0]
        for skill in ("asc", "cloakbrowser", "frida", "web-analysis"):
            self.assertIn(f"/opt/trace/.trace/skills/{skill}", removal)
        self.assertIn("FROM updated", hosted)
        pack = (root / "docker/pack-p2.sh").read_text()
        self.assertIn("capabilities.sha256 lightpanda.sha256;", pack)
        self.assertIn('"$root/scripts"', pack)
        self.assertIn('cp -- "$source_root/scripts/http-requirements.txt" "$root/scripts/http-requirements.txt"', pack)
        self.assertIn('sha256sum -c "$root/docker/lightpanda.sha256"', pack)
        self.assertIn('test -s "$root/.trace/skills/lightpanda/SKILL.md"', pack)

    def setUp(self):
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.calls = []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop)
        self.env = dict(os.environ, BENCHMARK_BASE_URL=f"http://127.0.0.1:{self.server.server_port}", BENCHMARK_TOKEN=TOKEN)

    def stop(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def invoke(self, operation, value, status=200, raw=False, cwd=None):
        self.server.reply = (status, value if raw else json.dumps(value, ensure_ascii=False).encode())
        command = [sys.executable, str(HELPER), operation]
        if operation != "list":
            command += ["--unique-code", "fixture /阶段"]
        if operation == "submit":
            command += ["--flag", "flag{字节}"]
        before = len(self.server.calls)
        proc = subprocess.run(command, env=self.env, cwd=cwd, capture_output=True, text=True, encoding="utf-8", timeout=20)
        self.assertEqual(len(self.server.calls), before + 1, "no automatic retry, redirect or cleanup calls")
        self.assertNotIn(TOKEN, proc.stdout + proc.stderr)
        record = json.loads(proc.stdout)
        self.assertEqual(record["http_status"], status)
        self.assertEqual(self.server.calls[-1][3], TOKEN)
        return proc.returncode, record

    def test_submit_nested_json_and_utf8_request(self):
        submitted = dict(correct=True, awarded=100, cumulative_score=300, correct_flag_count=1, total_flag_count=3, matched_flag_index=0)
        for depth in range(3):
            with self.subTest(depth=depth):
                value = submitted
                for _ in range(depth):
                    value = json.dumps(value)
                code, record = self.invoke("submit", value)
                self.assertEqual(code, 0)
                self.assertEqual(record["body"], submitted)
                self.assertEqual(record["assessment"]["cumulative_score"], 300)
                self.assertEqual(json.loads(self.server.calls[-1][2]), {"unique_code": "fixture /阶段", "flag": "flag{字节}"})
                self.assertNotIn("closed", record["assessment"])
        rejected = dict(submitted, correct=False, awarded=0, matched_flag_index=None)
        code, record = self.invoke("submit", rejected)
        self.assertEqual(code, 0)
        self.assertEqual(record["assessment"]["outcome"], "incorrect")
        self.assertEqual(record["assessment"]["awarded"], 0, "explicit zero is valid, unlike missing score")

    def test_submit_acceptance_is_separate_from_challenge_completion(self):
        partial = dict(correct=True, awarded=300, cumulative_score=300,
                       correct_flag_count=1, total_flag_count=4, matched_flag_index=0)
        _, receipt = self.invoke("submit", partial)
        self.assertTrue(receipt["assessment"]["submission_accepted"])
        self.assertFalse(receipt["assessment"]["challenge_completed"])
        self.assertEqual(receipt["assessment"]["remaining_flags"], 3)
        _, receipt = self.invoke("submit", dict(partial, cumulative_score=1200, correct_flag_count=4, matched_flag_index=3))
        self.assertTrue(receipt["assessment"]["submission_accepted"])
        self.assertTrue(receipt["assessment"]["challenge_completed"])
        self.assertEqual(receipt["assessment"]["remaining_flags"], 0)
        # The incident included a wrong submission after another owner solved it.
        _, receipt = self.invoke("submit", dict(partial, correct=False, awarded=0,
                                                cumulative_score=1200, correct_flag_count=4, matched_flag_index=None))
        self.assertFalse(receipt["assessment"]["submission_accepted"])
        self.assertTrue(receipt["assessment"]["challenge_completed"])
        _, receipt = self.invoke("submit", {"code": "duplicate"}, 409)
        self.assertNotIn("challenge_completed", receipt["assessment"])
        self.assertNotIn("submission_accepted", receipt["assessment"])
        self.assertTrue(all(call[1] == "/openapi/v1/challenges/submit" for call in self.server.calls),
                        "classification never closes an instance or retries a submission")

    def test_zero_total_does_not_count_as_completed_across_endpoints(self):
        row = dict(unique_code="fixture /阶段", total_score=0, flag_count=0,
                   correct_flag_count=0, is_completed=False, container_status="pending")
        code, progress = self.invoke("list", [row])
        self.assertEqual(code, 0)
        self.assertFalse(progress["assessment"]["progress"][0]["is_completed"])
        code, progress = self.invoke("list", [dict(row, is_completed=True)])
        self.assertEqual(code, 1)
        self.assertEqual(progress["assessment"]["outcome"], "unknown")
        self.assertIn("completion disagrees", progress["error"])
        body = dict(correct=False, awarded=0, cumulative_score=0,
                    correct_flag_count=0, total_flag_count=0, matched_flag_index=None)
        code, receipt = self.invoke("submit", body)
        self.assertEqual(code, 0)
        self.assertFalse(receipt["assessment"]["challenge_completed"])
        code, summary = self.summarize_receipts([receipt])
        self.assertEqual(code, 0)
        self.assertEqual(summary["completed_challenges"], 0)
        self.assertEqual(summary["correct_flags"], 0)

    def test_bad_score_and_body_never_become_zero(self):
        invalid = [b"HTTP 200\n{}", b"not-json", b"{\"awarded\":NaN}", b"{\"awarded\":1e999}", b"\xff", b"{}", json.dumps(json.dumps(json.dumps(json.dumps({})))).encode()]
        for raw in invalid:
            with self.subTest(raw=raw):
                code, record = self.invoke("submit", raw, raw=True)
                self.assertEqual(code, 1)
                self.assertEqual(record["assessment"]["outcome"], "unknown")
                self.assertIn("error", record)
                self.assertNotIn("cumulative_score", record["assessment"])

    def test_http_error_duplicate_and_redaction(self):
        for status, body, outcome in [(409, {"code": "duplicate"}, "duplicate"), (500, {"correct": True}, "http_error"), (409, {"code": "invalid_state", "message": TOKEN}, "http_error")]:
            code, record = self.invoke("submit", body, status)
            self.assertEqual(record["assessment"]["outcome"], outcome)
            self.assertEqual(code, 0 if outcome == "duplicate" else 1)
            self.assertNotIn("cumulative_score", record["assessment"])
        self.invoke("start", {}, 302)
        code, closed = self.invoke("close", {"closed": True}, 500)
        self.assertEqual(code, 1)
        self.assertIn("GET list", closed["next_action"])

    def test_independent_start_submit_progress_close(self):
        _, started = self.invoke("start", {"unique_code": "fixture /阶段", "container_addr": ["127.0.0.1:1"]})
        self.assertEqual(started["assessment"]["outcome"], "started")
        code, closed = self.invoke("close", {})
        self.assertEqual(code, 1)
        self.assertIn("GET list", closed["next_action"])
        row = dict(unique_code="fixture /阶段", total_score=300, flag_count=3, correct_flag_count=1, is_completed=False, container_status="stopped")
        code, progress = self.invoke("list", [row])
        self.assertEqual(code, 0)
        self.assertFalse(progress["assessment"]["progress"][0]["is_completed"])
        self.assertEqual(progress["assessment"]["progress"][0]["container_status"], "stopped")
        self.assertEqual(closed["assessment"]["outcome"], "unknown", "later GET must not rewrite original close evidence")
        _, receipt = self.invoke("close", {"unique_code": "fixture /阶段", "closed": True})
        self.assertEqual(receipt["assessment"]["outcome"], "closed")
        self.assertNotIn("correct_flag_count", receipt["assessment"])
        self.assertEqual([call[0] for call in self.server.calls], ["POST", "POST", "GET", "POST"])
        code, missing = self.invoke("list", [{"unique_code": "fixture"}])
        self.assertEqual(code, 1)
        self.assertIn("error", missing)
        self.assertIn("not earned", progress["assessment"]["score_semantics"])
        for field, value in [("flag_count", 3.5), ("correct_flag_count", 4), ("is_completed", "false"), ("is_completed", True), ("container_status", None)]:
            code, invalid = self.invoke("list", [dict(row, **{field: value})])
            self.assertEqual(code, 1)
            self.assertEqual(invalid["assessment"]["outcome"], "unknown")
        code, mismatch = self.invoke("close", {"unique_code": "another-challenge", "closed": True})
        self.assertEqual(code, 1)
        self.assertIn("GET list", mismatch["next_action"])

    def test_multi_flag_receipts_keep_latest_total_and_identity(self):
        first = dict(correct=True, awarded=100, cumulative_score=100, correct_flag_count=1, total_flag_count=3, matched_flag_index=0)
        duplicate = dict(first, awarded=0)
        second = dict(first, awarded=200, cumulative_score=300, correct_flag_count=2, matched_flag_index=2)
        for body in (first, duplicate, second):
            code, record = self.invoke("submit", body)
            self.assertEqual(code, 0)
            for field in ("awarded", "cumulative_score", "correct_flag_count", "total_flag_count", "matched_flag_index"):
                self.assertEqual(record["assessment"][field], body[field])
            self.assertNotIn("closed", record["assessment"])
        missing = dict(first)
        del missing["matched_flag_index"]
        invalid = [missing, dict(first, matched_flag_index=None), dict(first, matched_flag_index=True), dict(first, matched_flag_index=-1), dict(first, matched_flag_index=0.5), dict(first, correct_flag_count=0), dict(first, correct=False), dict(first, correct=False, matched_flag_index=None), dict(first, cumulative_score=99)]
        for body in invalid:
            with self.subTest(body=body):
                code, record = self.invoke("submit", body)
                self.assertEqual(code, 1)
                self.assertEqual(record["assessment"]["outcome"], "unknown")
                self.assertNotIn("cumulative_score", record["assessment"])
        row = dict(unique_code="fixture /阶段", total_score=300, flag_count=3, correct_flag_count=3, is_completed=True, container_status="available")
        code, record = self.invoke("list", [row])
        self.assertEqual(code, 0)
        self.assertEqual(record["assessment"]["progress"][0], row)
        code, record = self.invoke("list", [dict(row, is_completed=False)])
        self.assertEqual(code, 1)

    def summarize_receipts(self, receipts):
        before = len(self.server.calls)
        with tempfile.TemporaryDirectory(prefix="trace-score-") as tmp:
            path = Path(tmp, "receipts.json")
            path.write_text(json.dumps(receipts), encoding="utf-8")
            env = {k: v for k, v in self.env.items() if k not in ("BENCHMARK_BASE_URL", "BENCHMARK_TOKEN")}
            proc = subprocess.run([sys.executable, str(HELPER), "summary", "--receipts", str(path)], env=env, capture_output=True, text=True, encoding="utf-8", timeout=10)
        self.assertEqual(len(self.server.calls), before, "offline summary must not contact the platform")
        return proc.returncode, json.loads(proc.stdout)

    def test_score_summary_includes_partial_flags_and_deductions(self):
        receipts = []
        rows = [("a-01", 500), ("a-02", 450), ("a-03", 270), ("a-05", 100), ("a-06", 500), ("a-08", 500), ("a-10", 500), ("a-13", 500), ("a-15", 500), ("a-16", 500), ("a-18", 500), ("c-07", 100), ("d-02", 200), ("f1-05", 500), ("b-01", 300), ("b-03", 300), ("b-03", 600)]
        for i, (code, score) in enumerate(rows):
            body = dict(correct=True, awarded=300 if code.startswith("b-") else score, cumulative_score=score, correct_flag_count=2 if code == "b-03" and score == 600 else 1, total_flag_count=4 if code.startswith("b-") else 1, matched_flag_index=3 if score == 600 else 0)
            receipts.append(dict(endpoint="/openapi/v1/challenges/submit", method="POST", unique_code=code, observed_at=f"2026-11-14T08:{i:02d}:00Z", http_status=200, body=body))
        # File order and repeated copies must not double-count earlier totals.
        code, result = self.summarize_receipts(list(reversed(receipts)) + [receipts[-1]])
        self.assertEqual(code, 0, result)
        self.assertEqual((result["recorded_score"], result["correct_flags"], result["completed_challenges"]), (6520, 17, 14))
        self.assertEqual(len(result["challenges"]), 16)

    def test_score_summary_rejects_unknown_missing_or_conflicting_receipts(self):
        first = dict(endpoint="/openapi/v1/challenges/submit", method="POST", unique_code="fixture", observed_at="2026-11-14T08:00:00Z", http_status=200, body=dict(correct=True, awarded=100, cumulative_score=100, correct_flag_count=1, total_flag_count=3, matched_flag_index=0))
        duplicate = dict(first, observed_at="2026-11-14T08:01:00Z", http_status=409, body={"code": "duplicate"})
        code, result = self.summarize_receipts([first, duplicate])
        self.assertEqual((code, result.get("recorded_score")), (0, 100))
        same_stamp = dict(duplicate, observed_at=first["observed_at"])
        for receipts in ([first, same_stamp], [same_stamp, first], [duplicate, first]):
            code, result = self.summarize_receipts(receipts)
            self.assertEqual((code, result.get("recorded_score")), (0, 100))
        for receipts in ([], [duplicate], [dict(first, unique_code=None)], [dict(first, observed_at="2026-11-14T08:00:00")], [dict(first, http_status=True)], [dict(first, http_status=503)], [dict(first, body={})], [dict(first, error="capture incomplete")], [first, dict(duplicate, error="capture incomplete")], [first, dict(first, body=dict(first["body"], cumulative_score=200))]):
            with self.subTest(receipts=receipts):
                code, result = self.summarize_receipts(receipts)
                self.assertEqual((code, result["outcome"]), (1, "unknown"))
                self.assertNotIn("recorded_score", result)

    def test_shell_and_module_name_boundaries(self):
        for shell, code in [("sh", "set -- one two; printf '%s' \"$2\""), ("bash", "items=(one two); printf '%s' \"${items[1]}\"")]:
            proc = subprocess.run([shell, "-c", code], capture_output=True, text=True, timeout=5)
            self.assertEqual((proc.returncode, proc.stdout), (0, "two"))
        wrong = subprocess.run(["sh", "-c", "items=(one two)"], capture_output=True, timeout=5)
        self.assertNotEqual(wrong.returncode, 0)
        with tempfile.TemporaryDirectory(prefix="trace-contract-") as tmp:
            Path(tmp, "enum.py").write_text("raise RuntimeError('cwd module shadow')")
            code, record = self.invoke("hint", {"unique_code": "fixture /阶段", "hint": None}, cwd=tmp)
            self.assertEqual(code, 0, record)


if __name__ == "__main__":
    unittest.main(verbosity=2)
