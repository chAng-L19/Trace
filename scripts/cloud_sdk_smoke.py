"""Offline SDK wire-contract checks. Install optional SDKs or pass --sdk-path DIR."""
from __future__ import annotations

import argparse
import base64
import io
import json
import socket
import sys
from pathlib import Path
from unittest.mock import patch
from importlib.metadata import version

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from redteam_agent.runtime.cloud_inventory import cloud_inventory


BUNDLE = {"access_key_id": "fixture-access-key", "secret_access_key": "fixture-secret-key",
          "session_token": "fixture-session-token", "project_id": "fixture-project"}
REGIONS = {"huawei": "cn-north-4", "volcengine": "cn-beijing", "baidu": "bj", "jdcloud": "cn-north-1"}
PATHS = {"huawei": "/v1/fixture-project/cloudservers/detail", "volcengine": "Action=DescribeInstances",
         "baidu": "/v2/instance", "jdcloud": "/v1/regions/cn-north-1/instances"}
BODY = {
    "huawei": {"servers": [{"id": "vm-1", "name": "fixture", "status": "ACTIVE"}], "count": 1},
    "volcengine": {"ResponseMetadata": {"RequestId": "request-fixture"}, "Result": {
        "Instances": [{"InstanceId": "vm-1", "InstanceName": "fixture", "Status": "RUNNING"}], "TotalCount": 1}},
    "baidu": {"instances": [{"id": "vm-1", "name": "fixture", "status": "Running"}], "isTruncated": False},
    "jdcloud": {"requestId": "request-fixture", "result": {
        "instances": [{"instanceId": "vm-1", "instanceName": "fixture", "status": "running"}], "totalCount": 1}},
}


def invoke(provider, **kwargs):
    return cloud_inventory({"provider": provider, "region": REGIONS[provider], "credential_ref": json.dumps(BUNDLE),
                            "resource_types": ["compute"], "max_results": 10, **kwargs})["providers"][0]


def wire_check(provider, denied=False, operation="inventory"):
    import requests
    import urllib3

    seen = []
    status = 403 if denied else 200
    error = {"code": "AccessDenied", "message": "Permission denied", "requestId": "request-fixture"}
    body = error if denied else BODY[provider]
    if denied and provider == "huawei":
        body = {"error_code": "AccessDenied", "error_msg": "Permission denied"}
    elif denied and provider == "volcengine":
        body = {"ResponseMetadata": {"RequestId": "request-fixture", "Error": error}}
    elif denied and provider == "jdcloud":
        body = {"requestId": "request-fixture", "error": {**error, "status": 403}}
    content = json.dumps(body).encode()

    def record(method, url, headers):
        seen.append(str(url))
        assert str(method).upper() in {"GET", "B'GET'"}, method
        assert PATHS[provider] in str(url), url
        parameter = {"huawei": "limit=10", "volcengine": "MaxResults=10", "baidu": "maxKeys=10", "jdcloud": "pageSize=10"}[provider]
        assert parameter in str(url), url
        assert "authorization" in str(headers).lower(), headers
        token = BUNDLE["session_token"]
        if provider == "jdcloud":
            token = base64.b64encode(token.encode()).decode()
        assert token in str(headers), headers

    def requests_send(session, request, **kwargs):
        record(request.method, request.url, request.headers)
        response = requests.Response()
        response.status_code, response._content = status, content
        response.headers = {"Content-Type": "application/json", "X-Request-Id": "request-fixture"}
        response.request, response.url = request, request.url
        return response

    def pool_request(pool, method, url, **kwargs):
        from urllib.parse import urlencode
        record(method, url + "?" + urlencode(kwargs.get("fields") or {}), kwargs.get("headers"))
        return urllib3.response.HTTPResponse(body=content, status=status, headers={"Content-Type": "application/json"})

    class BceResponse(io.BytesIO):
        def __init__(self):
            super().__init__(content)
            self.status, self.reason = status, "Forbidden" if denied else "OK"

        def getheaders(self):
            return [("Content-Type", "application/json"), ("x-bce-request-id", "request-fixture")]

    class BceConnection:
        def close(self):
            pass

    def bce_send(connection, method, uri, headers, body, buffer_size):
        record(method, uri, headers)
        return BceResponse()

    with patch.object(socket.socket, "connect", side_effect=AssertionError("unexpected real network")), \
         patch("requests.sessions.Session.send", requests_send), \
         patch("urllib3.PoolManager.request", pool_request), \
         patch("baidubce.http.bce_http_client._get_connection", return_value=BceConnection()), \
         patch("baidubce.http.bce_http_client._send_http_request", bce_send):
        result = invoke(provider, operation=operation)
    assert seen, result
    assert result["status"] == ("permission_denied" if denied else "valid"), result
    assert result["identity_verified"] is False
    if not denied and operation == "inventory":
        assert result["resource_count"] == 1 and result["assets"][0]["id"] == "vm-1", result
        assert result["inventory_complete"] is True, result
    assert BUNDLE["secret_access_key"] not in json.dumps(result)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--sdk-path")
    args = parser.parse_args()
    if args.sdk_path:
        sys.path.insert(0, args.sdk_path)
    for provider in REGIONS:
        wire_check(provider)
        wire_check(provider, denied=True)
        wire_check(provider, operation="credential_check")
        wire_check(provider, operation="permission_check")
        with patch.dict("os.environ", {}, clear=True):
            missing = invoke(provider, credential_ref="")
        assert missing["status"] == "missing", missing
        with patch.dict("os.environ", {"BCE_ACCESS_KEY_ID": "ambient", "BCE_SECRET_ACCESS_KEY": "ambient"}):
            assert invoke(provider, credential_ref=json.dumps({"access_key_id": "partial"}))["status"] == "credential_ref_invalid"
        with patch("redteam_agent.runtime.cloud_sdk.compute", return_value={"instances": [], "next_token": "more"}):
            if provider != "huawei":
                result = invoke(provider)
                assert result["truncated"] and not result["inventory_complete"], result
        with patch("redteam_agent.runtime.cloud_sdk.compute", return_value={}):
            assert invoke(provider)["status"] == "malformed"
        with patch("redteam_agent.runtime.cloud_sdk.compute", side_effect=ModuleNotFoundError("sdk absent", name="fixture_sdk")):
            assert invoke(provider)["status"] == "sdk_backend_unavailable"
        with patch("redteam_agent.runtime.cloud_sdk.compute", side_effect=TimeoutError("connection timed out")):
            assert invoke(provider)["status"] == "endpoint_unreachable"
        with patch("redteam_agent.runtime.cloud_sdk.compute") as call:
            assert invoke(provider, credential_ref="not-json")["status"] == "credential_ref_invalid"
            assert invoke(provider, region="", credential_ref=json.dumps({**BUNDLE, "region": "host.invalid/path"}))["status"] == "credential_ref_invalid"
            call.assert_not_called()
        with patch("redteam_agent.runtime.cloud_sdk.compute", return_value=({"servers": [], "count": 0} if provider == "huawei" else {"result": {"instances": [], "totalCount": 0}} if provider == "jdcloud" else {"instances": []})):
            result = invoke(provider, resource_types=["storage"])
            assert result["status"] == "inventory_failed" and result["failures"] == [{"kind": "storage", "status": "unsupported"}], result
    print(json.dumps({"status": "passed", "providers": list(REGIONS), "real_network": False,
                      "sdk_versions": {name: version(name) for name in ("huaweicloudsdkecs", "volcengine-python-sdk", "bce-python-sdk", "jdcloud-sdk")},
                      "checks": ["signed_requests", "session_tokens", "response_projection", "permission_errors",
                                 "missing_credentials", "pagination", "malformed_response", "missing_sdk", "timeout", "unsupported_scope"]}))


if __name__ == "__main__":
    main()
