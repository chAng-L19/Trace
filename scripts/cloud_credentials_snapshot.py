from __future__ import annotations

import json
import os
import sys
from pathlib import Path
from unittest.mock import patch


PROJECT_ROOT = Path(__file__).resolve().parents[1]
SRC_ROOT = PROJECT_ROOT / "src"
if str(SRC_ROOT) not in sys.path:
    sys.path.insert(0, str(SRC_ROOT))

from redteam_agent.runtime import cloud_inventory as cloud_tools  # noqa: E402


def _process(stdout: str = "", stderr: str = "", return_code: int = 0):
    return type("Process", (), {"stdout": stdout, "stderr": stderr, "returncode": return_code})()


def _validation_checks() -> list[str]:
    cases = (
        ("aws", {}, "malformed"),
        ("aws", [], "malformed"),
        ("aws", {"Account": {}, "Arn": "arn:aws:iam::123456789012:role/trace"}, "malformed"),
        ("azure", {}, "malformed"),
        ("gcp", [], "missing"),
        ("gcp", [{"account": "inactive@example.test", "status": "INACTIVE"}], "missing"),
        ("gcp", [{"account": {}, "status": "ACTIVE"}], "malformed"),
        ("tencent", {"Response": {}}, "malformed"),
        ("aliyun", {}, "malformed"),
    )
    for provider, payload, expected in cases:
        with patch.object(cloud_tools, "resolve_executable", return_value="fixture"), \
             patch.object(cloud_tools.subprocess, "run", return_value=_process(json.dumps(payload))) as invoked:
            result = cloud_tools.cloud_inventory({"provider": provider, "operation": "credential_check"})["providers"][0]
        assert result["status"] == expected, (provider, expected, result["status"])
        assert not result["principal"] and not result["identity_verified"]
        assert invoked.call_count == 1

    for provider, identity in (
        ("gcp", [{"account": "active@example.test", "status": "ACTIVE"}]),
        ("azure", {"id": "subscription-fixture", "user": {"name": "active@example.test"}}),
    ):
        for probe, expected in ((_process("[]"), "valid"),
                                (_process("{}", "ExpiredToken: token has expired", 1), "expired")):
            with patch.object(cloud_tools, "resolve_executable", return_value="fixture"), \
                 patch.object(cloud_tools.subprocess, "run", side_effect=[_process(json.dumps(identity)), probe]) as invoked:
                result = cloud_tools.cloud_inventory({"provider": provider, "operation": "credential_check"})["providers"][0]
            assert result["status"] == expected
            assert result["return_code"] == probe.returncode
            assert not result["identity_verified"]
            assert invoked.call_count == 2

    token = json.dumps({"access_token": "SENTINEL_CREDENTIAL_DO_NOT_PRINT"})
    for operation in ("credential_check", "permission_check", "inventory"):
        with patch.object(cloud_tools, "resolve_executable", return_value="fixture"), \
             patch.object(cloud_tools.subprocess, "run", return_value=_process("[]")) as invoked:
            result = cloud_tools.cloud_inventory({"provider": "gcp", "operation": operation,
                                                       "credential_ref": token, "resource_types": ["compute"]})["providers"][0]
        assert result["status"] == "valid" and not result["principal"]
        assert invoked.call_args_list[0].args[0][1:3] == ("projects", "list")
        assert invoked.call_args_list[0].kwargs["env"]["CLOUDSDK_AUTH_ACCESS_TOKEN"] == "SENTINEL_CREDENTIAL_DO_NOT_PRINT"
        assert all(call.kwargs["env"]["PYTHONDONTWRITEBYTECODE"] == "1" for call in invoked.call_args_list)

    for operation in ("credential_check", "permission_check", "inventory"):
        def ambient_process(command, **kwargs):
            return _process(json.dumps([{"account": "active@example.test", "status": "ACTIVE"}]) if command[1:3] == ("auth", "list") else "[]")
        with patch.object(cloud_tools, "resolve_executable", return_value="fixture"), \
             patch.object(cloud_tools.subprocess, "run", side_effect=ambient_process) as invoked:
            result = cloud_tools.cloud_inventory({"provider": "gcp", "operation": operation, "resource_types": ["compute"]})["providers"][0]
        assert result["status"] == "valid"
        assert all(call.kwargs["env"]["PYTHONDONTWRITEBYTECODE"] == "1" for call in invoked.call_args_list)

    bundle = json.dumps({"access_key_id": "FIXTURE_ID", "secret_access_key": "FIXTURE_SECRET"})
    with patch.dict(os.environ, {"AWS_SESSION_TOKEN": "ambient-token", "AWS_PROFILE": "ambient-profile",
                                "AWS_ENDPOINT_URL": "https://ambient.invalid", "AWS_ENDPOINT_URL_STS": "https://ambient.invalid",
                                "AWS_CONFIG_FILE": "ambient-config", "AWS_SHARED_CREDENTIALS_FILE": "ambient-credentials"}, clear=True):
        env = cloud_tools._cloud_environment("aws", bundle)
    assert not {"AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS"}.intersection(env)
    assert env["AWS_IGNORE_CONFIGURED_ENDPOINT_URLS"] == "true"
    assert env["AWS_CONFIG_FILE"] == env["AWS_SHARED_CREDENTIALS_FILE"] == os.devnull
    with patch.dict(os.environ, {"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "ambient@example.test",
                                "CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER": "https://ambient.invalid"}, clear=True):
        env = cloud_tools._cloud_environment("gcp", token)
    assert "CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT" not in env
    assert "CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER" not in env
    return ["identity_shape", "inactive_account_rejection", "remote_read_verification",
            "explicit_token_identity_isolation", "ambient_endpoint_isolation", "stderr_classification", "managed_cli_bytecode_disabled"]


def _run() -> dict[str, object]:
    aws_identity = json.dumps({"Account": "123456789012", "Arn": "arn:aws:iam::123456789012:role/trace"})
    calls = iter(
        (
            _process(aws_identity),
            _process(stderr="ExpiredToken: token has expired", return_code=1),
            _process(stderr="Unable to locate credentials", return_code=1),
        )
    )
    with patch.object(cloud_tools, "resolve_executable", side_effect=lambda name: name if name in {"aws", "az", "gcloud"} else None), \
         patch.object(cloud_tools.subprocess, "run", side_effect=lambda *args, **kwargs: next(calls)), \
         patch.object(cloud_tools.time, "monotonic", return_value=10.0), \
         patch.object(cloud_tools.cloud_sdk, "credentials", return_value={}):
        result = cloud_tools.cloud_inventory({"operation": "credential_check", "provider": "auto"})
    providers = result["providers"]
    assert result["secret_exposed"] is False
    assert providers[0]["status"] == "valid"
    assert providers[0]["account_id"] == "123456789012"
    assert providers[0]["principal"].endswith("trace")
    assert providers[1]["status"] == "expired"
    assert providers[2]["status"] == "missing"
    assert "ExpiredToken" not in json.dumps(result)
    assert {item["provider"] for item in providers[3:]} == {"tencent", "aliyun", "huawei", "volcengine", "baidu", "jdcloud"}
    assert all(item["status"] == "provider_unavailable" for item in providers[3:5])
    assert all(item["status"] == "missing" for item in providers[5:])
    with patch.object(cloud_tools, "resolve_executable", return_value=None):
        unavailable = cloud_tools.cloud_inventory({"operation": "credential_check", "provider": "aws"})
    assert unavailable["providers"][0]["status"] == "provider_unavailable"
    seen_environment: dict[str, str] = {}
    credential = json.dumps({
        "access_key_id": "AKIAEXAMPLE",
        "secret_access_key": "SENTINEL_CREDENTIAL_DO_NOT_PRINT",
        "session_token": "temporary-token",
    })
    with patch.object(cloud_tools, "resolve_executable", side_effect=lambda name: name), \
         patch.object(cloud_tools.subprocess, "run", side_effect=lambda *args, **kwargs: (seen_environment.update(kwargs["env"]) or _process(aws_identity))), \
         patch.object(cloud_tools.time, "monotonic", return_value=40.0):
        bound = cloud_tools.cloud_inventory({"operation": "credential_check", "provider": "aws", "credential_ref": credential})
    assert seen_environment["AWS_SECRET_ACCESS_KEY"] == "SENTINEL_CREDENTIAL_DO_NOT_PRINT"
    assert "SENTINEL_CREDENTIAL_DO_NOT_PRINT" not in json.dumps(bound)
    permission_calls: list[tuple[str, ...]] = []
    def permission_run(*args, **kwargs):
        permission_calls.append(tuple(args[0]))
        return _process("[]")

    with patch.object(cloud_tools, "resolve_executable", return_value="aws"), \
         patch.object(cloud_tools.subprocess, "run", side_effect=permission_run), \
         patch.object(cloud_tools.time, "monotonic", return_value=50.0):
        permission = cloud_tools.cloud_inventory({"operation": "permission_check", "provider": "aws"})
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
        "validation_checks": _validation_checks(),
    }


if __name__ == "__main__":
    print(json.dumps(_run(), ensure_ascii=False, indent=2, sort_keys=True))
