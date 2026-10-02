from __future__ import annotations

import json
import sys
from pathlib import Path
from unittest.mock import patch


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.runtime import open_source_tools  # noqa: E402


def _process(stdout: str = "", stderr: str = "", return_code: int = 0):
    return type("Process", (), {"stdout": stdout, "stderr": stderr, "returncode": return_code})()


def _run() -> dict[str, object]:
    aws_identity = json.dumps({"Account": "123456789012", "Arn": "arn:aws:iam::123456789012:role/trace"})
    calls = iter(
        (
            _process(aws_identity),
            _process(stderr="ExpiredToken: token has expired", return_code=1),
            _process(stderr="Unable to locate credentials", return_code=1),
        )
    )
    with patch.object(open_source_tools, "resolve_executable", side_effect=lambda name: name if name in {"aws", "az", "gcloud"} else None), \
         patch.object(open_source_tools.subprocess, "run", side_effect=lambda *args, **kwargs: next(calls)), \
         patch.object(open_source_tools.time, "monotonic", return_value=10.0):
        result = open_source_tools.cloud_inventory({"operation": "credential_check", "provider": "auto"})
    providers = result["providers"]
    assert result["secret_exposed"] is False
    assert providers[0]["status"] == "valid"
    assert providers[0]["account_id"] == "123456789012"
    assert providers[0]["principal"].endswith("trace")
    assert providers[1]["status"] == "expired"
    assert providers[2]["status"] == "missing"
    assert "ExpiredToken" not in json.dumps(result)
    assert {item["provider"] for item in providers[3:]} == {"tencent", "aliyun", "huawei", "volcengine", "baidu", "jdcloud"}
    assert all(item["status"] == "provider_unavailable" for item in providers[3:])
    with patch.object(open_source_tools, "resolve_executable", return_value=None):
        unavailable = open_source_tools.cloud_inventory({"operation": "credential_check", "provider": "aws"})
    assert unavailable["providers"][0]["status"] == "provider_unavailable"
    seen_environment: dict[str, str] = {}
    credential = json.dumps({
        "access_key_id": "AKIAEXAMPLE",
        "secret_access_key": "SENTINEL_CREDENTIAL_DO_NOT_PRINT",
        "session_token": "temporary-token",
    })
    with patch.object(open_source_tools, "resolve_executable", side_effect=lambda name: name), \
         patch.object(open_source_tools.subprocess, "run", side_effect=lambda *args, **kwargs: (seen_environment.update(kwargs["env"]) or _process(aws_identity))), \
         patch.object(open_source_tools.time, "monotonic", return_value=40.0):
        bound = open_source_tools.cloud_inventory({"operation": "credential_check", "provider": "aws", "credential_ref": credential})
    assert seen_environment["AWS_SECRET_ACCESS_KEY"] == "SENTINEL_CREDENTIAL_DO_NOT_PRINT"
    assert "SENTINEL_CREDENTIAL_DO_NOT_PRINT" not in json.dumps(bound)
    permission_calls: list[tuple[str, ...]] = []
    def permission_run(*args, **kwargs):
        permission_calls.append(tuple(args[0]))
        return _process("[]")

    with patch.object(open_source_tools, "resolve_executable", return_value="aws"), \
         patch.object(open_source_tools.subprocess, "run", side_effect=permission_run), \
         patch.object(open_source_tools.time, "monotonic", return_value=50.0):
        permission = open_source_tools.cloud_inventory({"operation": "permission_check", "provider": "aws"})
    assert permission["providers"][0]["status"] == "valid"
    assert permission_calls[0][1:3] == ("ec2", "describe-regions")
    return {
        "schema_version": 1,
        "operation": result["operation"],
        "statuses": [item["status"] for item in providers],
        "identity_fields": sorted(key for key in providers[0] if key in {"account_id", "principal", "credential_type", "region", "expires_at", "request_id"}),
        "secret_exposed": result["secret_exposed"],
        "unavailable_status": unavailable["providers"][0]["status"],
        "credential_ref_injected_to_child": seen_environment["AWS_SECRET_ACCESS_KEY"] == "SENTINEL_CREDENTIAL_DO_NOT_PRINT",
        "permission_status": permission["providers"][0]["status"],
    }


if __name__ == "__main__":
    print(json.dumps(_run(), ensure_ascii=False, indent=2, sort_keys=True))
