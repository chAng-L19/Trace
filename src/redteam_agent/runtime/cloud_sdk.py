"""Optional official SDK backends for cloud-inventory's read-only compute probe."""
from __future__ import annotations

import json
import logging
import os
from typing import Any, Mapping


SDK_PACKAGES = {
    "huawei": "huaweicloudsdkecs", "volcengine": "volcengine-python-sdk",
    "baidu": "bce-python-sdk", "jdcloud": "jdcloud-sdk",
}
_ENV_PREFIX = {"huawei": "HUAWEICLOUD", "volcengine": "VOLCENGINE", "baidu": "BCE", "jdcloud": "JDCLOUD"}


def credentials(provider: str, reference: Any) -> dict[str, str]:
    if reference:
        try:
            bundle = json.loads(str(reference))
        except ValueError:
            raise ValueError("credential_ref_malformed") from None
        if not isinstance(bundle, dict) or any(not isinstance(value, str) for value in bundle.values()):
            raise ValueError("credential_ref_malformed")
        return {key.casefold().replace("-", "_"): value for key, value in bundle.items()}
    prefix = _ENV_PREFIX[provider]
    return {key: os.environ.get(f"{prefix}_{key.upper()}", "") for key in
            ("access_key_id", "secret_access_key", "session_token", "region", "project_id")}


def compute(provider: str, bundle: Mapping[str, str], region: str, limit: int, timeout: float) -> dict[str, Any]:
    """Build/sign requests with vendor SDKs; imports never affect the default install."""
    ak, sk, token = bundle["access_key_id"], bundle["secret_access_key"], bundle.get("session_token", "")
    if provider == "huawei":
        from huaweicloudsdkcore.auth.credentials import BasicCredentials
        from huaweicloudsdkcore.http.http_config import HttpConfig
        from huaweicloudsdkecs.v2 import EcsClient, ListServersDetailsRequest
        from huaweicloudsdkecs.v2.region.ecs_region import EcsRegion

        credential = BasicCredentials(ak, sk, bundle["project_id"])
        if token:
            credential = credential.with_security_token(token)
        config = HttpConfig.get_default_config()
        config.timeout = timeout
        client = EcsClient.new_builder().with_credentials(credential).with_region(EcsRegion.value_of(region)).with_http_config(config).build()
        response = client.list_servers_details(ListServersDetailsRequest(limit=limit))
        return response.to_dict()
    if provider == "volcengine":
        import volcenginesdkcore
        import volcenginesdkecs

        config = volcenginesdkcore.Configuration()
        config.ak, config.sk, config.session_token, config.region = ak, sk, token, region
        client = volcenginesdkecs.ECSApi(volcenginesdkcore.ApiClient(config))
        response = client.describe_instances(volcenginesdkecs.DescribeInstancesRequest(max_results=limit), _request_timeout=timeout)
        return response.to_dict()
    if provider == "baidu":
        from baidubce.auth.bce_credentials import BceCredentials
        from baidubce.bce_client_configuration import BceClientConfiguration
        from baidubce.services.bcc.bcc_client import BccClient

        config = BceClientConfiguration(credentials=BceCredentials(ak, sk), endpoint=f"https://bcc.{region}.baidubce.com",
                                       security_token=token or None, connection_timeout_in_mills=int(timeout * 1000))
        # BCC list_instances does not forward config.security_token. Use its same
        # official signed request path, explicitly carrying the STS header.
        response = BccClient(config)._send_request(b"GET", b"/instance", params={"maxKeys": limit},
                                                  headers={b"x-bce-security-token": token.encode()} if token else None)
        return _plain(response)
    from jdcloud_sdk.core.config import Config
    from jdcloud_sdk.core.credential import Credential
    from jdcloud_sdk.services.vm.apis.DescribeInstancesRequest import DescribeInstancesParameters, DescribeInstancesRequest
    from jdcloud_sdk.services.vm.client.VmClient import VmClient

    parameters = DescribeInstancesParameters(region)
    parameters.setPageSize(limit)
    # The vendor client's default logger logs signed headers; use a private null logger.
    logger = logging.Logger("trace.jdcloud", level=logging.CRITICAL + 1)
    logger.addHandler(logging.NullHandler())
    client = VmClient(Credential(ak, sk), Config("vm.jdcloud-api.com", timeout=timeout), logger=logger)
    response = client.send(DescribeInstancesRequest(parameters, header={"x-jdcloud-security-token": token} if token else None))
    return _plain(response)


def _plain(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {key: _plain(item) for key, item in value.items()}
    if isinstance(value, (tuple, list)):
        return [_plain(item) for item in value]
    if hasattr(value, "__dict__"):
        return {key: _plain(item) for key, item in vars(value).items() if not key.startswith("_")}
    return value
