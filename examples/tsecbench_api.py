#!/usr/bin/env python3
"""One benchmark HTTP request; no retry, scheduling, or inferred success."""
import argparse
from datetime import datetime, timezone
from decimal import Decimal
import json
import math
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None  # Never forward the benchmark credential to a redirect.


def decode_body(raw):
    def invalid_constant(value):
        raise ValueError("invalid JSON constant: " + value)
    def finite_float(value):
        number = float(value)
        if not math.isfinite(number):
            raise ValueError("nonfinite JSON number")
        return number
    value = json.loads(raw, parse_constant=invalid_constant, parse_float=finite_float)
    for _ in range(2):
        if not isinstance(value, str):
            break
        value = json.loads(value, parse_constant=invalid_constant, parse_float=finite_float)
    if not isinstance(value, (dict, list)):
        raise ValueError("expected JSON object/array after at most two string layers")
    return value


def assess(operation, status, body):
    result = {"operation": operation, "outcome": "unknown"}
    if not 200 <= status < 300:
        result["outcome"] = "duplicate" if operation == "submit" and status == 409 and isinstance(body, dict) and body.get("code") == "duplicate" else "http_error"
        return result
    if operation == "list":
        if not isinstance(body, list):
            raise ValueError("list response must be an array")
        result["progress"] = []
        result["score_semantics"] = "total_score is the challenge maximum, not earned score; do not sum it as earned points"
        for row in body:
            if not isinstance(row, dict) or not isinstance(row.get("unique_code"), str):
                raise ValueError("invalid progress row")
            for key in ("total_score", "flag_count", "correct_flag_count"):
                if type(row.get(key)) not in (int, float) or not math.isfinite(row[key]) or row[key] < 0:
                    raise ValueError("missing/invalid progress field: " + key)
            if any(type(row[key]) is not int for key in ("flag_count", "correct_flag_count")) or row["correct_flag_count"] > row["flag_count"]:
                raise ValueError("invalid progress flag counts")
            if type(row.get("is_completed")) is not bool or row.get("container_status") not in ("pending", "available", "stop_pending", "stopped"):
                raise ValueError("missing/invalid progress state")
            if row["is_completed"] != (row["flag_count"] > 0 and row["correct_flag_count"] == row["flag_count"]):
                raise ValueError("progress completion disagrees with flag counts")
            result["progress"].append({key: row.get(key) for key in ("unique_code", "total_score", "flag_count", "correct_flag_count", "is_completed", "container_status")})
        result["outcome"] = "observed"
        return result
    if not isinstance(body, dict):
        raise ValueError("endpoint response must be an object")
    if operation == "submit":
        for key in ("awarded", "cumulative_score", "correct_flag_count", "total_flag_count"):
            value = body.get(key)
            result[key] = value
            if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
                raise ValueError("missing/invalid submit field: " + key)
        if type(body.get("correct")) is not bool:
            raise ValueError("missing/invalid submit field: correct")
        if any(type(body[key]) is not int for key in ("correct_flag_count", "total_flag_count")) or body["correct_flag_count"] > body["total_flag_count"]:
            raise ValueError("invalid submit flag counts")
        if "matched_flag_index" not in body:
            raise ValueError("missing submit field: matched_flag_index")
        matched = body["matched_flag_index"]
        if body["correct"]:
            if type(matched) is not int or matched < 0 or body["correct_flag_count"] == 0:
                raise ValueError("correct submit requires a matched flag and positive correct count")
        elif matched is not None or body["awarded"] != 0:
            raise ValueError("incorrect submit requires null matched flag and zero awarded")
        if body["awarded"] > body["cumulative_score"]:
            raise ValueError("awarded exceeds cumulative score")
        # Platform docs do not define the index base; retain its exact identity.
        result["matched_flag_index"] = matched
        result["score_semantics"] = "cumulative_score is this challenge's latest total, never add historical cumulative values"
        result["outcome"] = "correct" if body["correct"] else "incorrect"
        # Submission acceptance and multi-flag completion are independent.
        result["submission_accepted"] = body["correct"]
        result["challenge_completed"] = body["total_flag_count"] > 0 and body["correct_flag_count"] == body["total_flag_count"]
        result["remaining_flags"] = body["total_flag_count"] - body["correct_flag_count"]
    elif operation == "close":
        result["outcome"] = "closed" if body.get("closed") is True else "unknown"
    elif operation == "start":
        addresses = body.get("container_addr")
        if isinstance(addresses, list) and addresses and all(isinstance(x, str) and x for x in addresses):
            result["outcome"] = "started"
    elif operation == "hint" and "hint" in body:
        result["outcome"] = "observed"
    return result


def summarize(receipts):
    """Derived receipt view only; no requests, persistence or inferred points."""
    # Muteki 3b8fd7abe4ee competition/submission.py:9-25: bind receipts to the
    # submitted identity and keep unknown separate. This is an independent,
    # offline projection, not its competition store or submission scheduler.
    if not isinstance(receipts, list) or not receipts:
        raise ValueError("nonempty submit receipt array required; missing receipts are not zero points")
    latest = {}
    duplicates = set()
    ordered = []
    for record in receipts:
        if not isinstance(record, dict) or record.get("endpoint") != "/openapi/v1/challenges/submit" or record.get("method") != "POST":
            raise ValueError("summary accepts saved submit receipts only")
        code, observed = record.get("unique_code"), record.get("observed_at")
        if not isinstance(code, str) or not code or not isinstance(observed, str):
            raise ValueError("receipt requires requested unique_code and observed_at; do not infer identity")
        stamp = datetime.fromisoformat(observed.replace("Z", "+00:00"))
        if stamp.utcoffset() is None:
            raise ValueError("observed_at requires an explicit timezone")
        if record.get("error"):
            raise ValueError("incomplete submit receipt for " + code)
        ordered.append((stamp, record))
    for stamp, record in sorted(ordered, key=lambda pair: pair[0]):
        code = record["unique_code"]
        status = record.get("http_status")
        if type(status) is not int:
            raise ValueError("receipt requires an HTTP status")
        observed = assess("submit", status, record.get("body"))
        if observed["outcome"] == "duplicate":
            duplicates.add(code)
            continue  # Resolve its missing total from the full receipt set, not file order.
        if observed["outcome"] not in ("correct", "incorrect"):
            raise ValueError("unresolved submit receipt for " + code + "; do not replace unknown points with zero")
        value = {key: observed[key] for key in ("cumulative_score", "correct_flag_count", "total_flag_count")}
        if code in latest and latest[code][0] == stamp and latest[code][1] != value:
            raise ValueError("conflicting submit receipts at the same observed_at for " + code)
        latest[code] = (stamp, value)
    if duplicates - latest.keys():
        raise ValueError("duplicate receipt has no cumulative score for " + ", ".join(sorted(duplicates - latest.keys())))
    score = sum((Decimal(str(value["cumulative_score"])) for _, value in latest.values()), Decimal(0))
    return {"outcome": "observed", "recorded_score": float(score) if score % 1 else int(score),
            "correct_flags": sum(value["correct_flag_count"] for _, value in latest.values()),
            "completed_challenges": sum(value["total_flag_count"] > 0 and value["correct_flag_count"] == value["total_flag_count"] for _, value in latest.values()),
            "challenges": [{"unique_code": code, **value} for code, (_, value) in sorted(latest.items())],
            "score_semantics": "Sum the latest cumulative_score per challenge in the supplied receipts, including partial flags and deductions. This is recorded raw points, not a normalized platform score; missing challenges remain unknown."}


def request(operation, unique_code=None, flag=None):
    base, token = os.environ.get("BENCHMARK_BASE_URL", ""), os.environ.get("BENCHMARK_TOKEN", "")
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme not in ("http", "https") or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment or not token:
        raise ValueError("valid BENCHMARK_BASE_URL and BENCHMARK_TOKEN environment required")
    endpoint = "/openapi/v1/challenges" + ("" if operation == "list" else "/" + operation)
    if operation not in ("list", "submit"):
        endpoint += "?" + urllib.parse.urlencode({"unique_code": unique_code})
    data = json.dumps({"unique_code": unique_code, "flag": flag}, ensure_ascii=False).encode("utf-8") if operation == "submit" else None
    method = "GET" if operation in ("list", "hint") else "POST"
    req = urllib.request.Request(base.rstrip("/") + endpoint, data=data, method=method, headers={"BENCHMARK_TOKEN": token, "Content-Type": "application/json"})
    result = {"endpoint": endpoint, "method": method, "unique_code": unique_code, "http_status": None, "raw_body": None, "body": None, "assessment": {"operation": operation, "outcome": "unknown"}}
    try:
        try:
            response = urllib.request.build_opener(NoRedirect()).open(req, timeout=15)
        except urllib.error.HTTPError as exc:
            response = exc
        with response:
            result["http_status"] = response.code
            raw = response.read(4 * 1024 * 1024 + 1)
        result["raw_body"] = raw.decode("utf-8", errors="replace")
        if len(raw) > 4 * 1024 * 1024:
            raise ValueError("body exceeds 4MiB capture limit; truncated, not authoritative")
        result["body"] = decode_body(raw.decode("utf-8"))
        if 200 <= result["http_status"] < 300 and operation in ("start", "close", "hint") and (not isinstance(result["body"], dict) or result["body"].get("unique_code") != unique_code):
            raise ValueError("response unique_code missing or differs from requested challenge")
        result["assessment"] = assess(operation, result["http_status"], result["body"])
    except (ValueError, OSError, OverflowError) as exc:
        result["error"] = str(exc)
    if operation == "close" and result["assessment"]["outcome"] != "closed":
        result["next_action"] = "GET list and inspect this unique_code container_status before deciding whether to retry; do not infer cleanup"
    result["observed_at"] = datetime.now(timezone.utc).isoformat()
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("list", "start", "submit", "close", "hint", "summary"))
    parser.add_argument("--unique-code")
    parser.add_argument("--flag")
    parser.add_argument("--receipts", help="JSON array of saved submit receipts; summary is offline")
    args = parser.parse_args()
    if args.operation == "summary":
        if not args.receipts or args.unique_code or args.flag:
            parser.error("summary requires --receipts and accepts no --unique-code/--flag")
        try:
            with open(args.receipts, encoding="utf-8") as source:
                print(json.dumps(summarize(json.load(source)), ensure_ascii=False, allow_nan=False))
            return 0
        except (ValueError, OSError, OverflowError) as exc:
            print(json.dumps({"outcome": "unknown", "error": str(exc)}, ensure_ascii=False))
            return 1
    if args.receipts:
        parser.error("--receipts is only used by summary")
    if args.operation != "list" and not args.unique_code:
        parser.error("--unique-code required")
    if args.operation == "submit" and (not args.flag or len(args.flag) > 4096):
        parser.error("--flag of length 1..4096 required")
    try:
        result = request(args.operation, args.unique_code, args.flag)
    except ValueError as exc:
        result = {"error": str(exc)}
    # Redact raw and parsed echoes alike without breaking JSON escaping.
    token = os.environ.get("BENCHMARK_TOKEN", "")
    def redact(value):
        if isinstance(value, str):
            return value.replace(token, "[REDACTED]") if token else value
        if isinstance(value, list):
            return [redact(item) for item in value]
        if isinstance(value, dict):
            return {redact(key): redact(item) for key, item in value.items()}
        return value
    print(json.dumps(redact(result), ensure_ascii=False, allow_nan=False))
    return 0 if result.get("assessment", {}).get("outcome") in ("observed", "started", "correct", "incorrect", "closed", "duplicate") else 1


if __name__ == "__main__":
    sys.exit(main())
