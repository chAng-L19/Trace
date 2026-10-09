from __future__ import annotations

import json
import base64
import math
import os
import re
import subprocess
import time
from typing import Any, Mapping

from .managed_tools import resolve_executable
from . import cloud_sdk


_CLOUD_COMMANDS: dict[str, tuple[str, ...]] = {
    "aws": ("aws", "sts", "get-caller-identity", "--output", "json"),
    "azure": ("az", "account", "show", "--output", "json"),
    "gcp": ("gcloud", "auth", "list", "--format=json"),
    "tencent": ("tccli", "sts", "GetCallerIdentity"),
    "aliyun": ("aliyun", "sts", "GetCallerIdentity"),
}

_CLOUD_PERMISSION_COMMANDS: dict[str, tuple[str, ...]] = {
    "aws": ("aws", "ec2", "describe-regions", "--output", "json"),
    "azure": ("az", "group", "list", "--output", "json"),
    "gcp": ("gcloud", "projects", "list", "--limit=1", "--format=json"),
    "tencent": ("tccli", "cvm", "DescribeRegions"),
    "aliyun": ("aliyun", "ecs", "DescribeRegions"),
}

_CLOUD_INVENTORY_COMMANDS: dict[str, dict[str, tuple[str, ...]]] = {
    "aws": {
        "regions": ("ec2", "describe-regions", "--all-regions", "--output", "json"),
        "compute": ("ec2", "describe-instances", "--output", "json"),
        "storage": ("s3api", "list-buckets", "--output", "json"),
        "iam": ("iam", "list-roles", "--output", "json", "--max-items", "100"),
    },
    "azure": {
        "regions": ("account", "list-locations", "--output", "json"),
        "resources": ("resource", "list", "--output", "json"),
        "compute": ("vm", "list", "--show-details", "--output", "json"),
        "storage": ("storage", "account", "list", "--output", "json"),
    },
    "gcp": {
        "regions": ("compute", "regions", "list", "--format=json"),
        "resources": ("asset", "search-all-resources", "--format=json"),
        "compute": ("compute", "instances", "list", "--format=json"),
        "storage": ("storage", "buckets", "list", "--format=json"),
    },
    "tencent": {
        "regions": ("cvm", "DescribeRegions"),
        "compute": ("cvm", "DescribeInstances", "--Limit", "100"),
    },
    "aliyun": {
        "regions": ("ecs", "DescribeRegions"),
        "compute": ("ecs", "DescribeInstances", "--PageSize", "100"),
    },
}

_CLOUD_INVENTORY_ALIASES = {"all": "resources", "assets": "resources", "instances": "compute", "buckets": "storage"}
_CLOUD_INVENTORY_DEFAULTS = ("regions", "compute", "storage")
_CLOUD_MAX_RECORDS = 200

_CLOUD_EXECUTABLES = {
    "aws": "aws", "azure": "az", "gcp": "gcloud", "tencent": "tccli", "aliyun": "aliyun",
    "huawei": "hcloud", "volcengine": "ve", "baidu": "bce", "jdcloud": "jdc",
}

_CLOUD_ALIASES = {
    "alibaba": "aliyun", "alibaba_cloud": "aliyun", "huaweicloud": "huawei",
    "volc": "volcengine", "bce": "baidu", "jd": "jdcloud",
}

def _cloud_status(return_code: int, output: str, payload: Any) -> str:
    if isinstance(payload, Mapping):
        response = payload.get("Response", payload)
        if isinstance(response, Mapping) and (response.get("Error") or response.get("Code")):
            return_code = return_code or 1
    if return_code == 0:
        return "valid" if isinstance(payload, (Mapping, list)) else "malformed"
    lowered = output.casefold()
    if any(marker in lowered for marker in ("expired", "expiration", "token has expired")):
        return "expired"
    if any(marker in lowered for marker in ("signaturedoesnotmatch", "signature rejected", "invalid signature")):
        return "signature_rejected"
    if any(marker in lowered for marker in ("accessdenied", "access denied", "permission denied", "forbidden", "unauthorized")):
        return "permission_denied"
    if any(marker in lowered for marker in ("credential", "not logged in", "no active account", "authentication required")):
        return "missing"
    if any(marker in lowered for marker in ("timed out", "timeout", "connection", "unreachable", "could not resolve", "network")):
        return "endpoint_unreachable"
    if any(marker in lowered for marker in ("invalid", "malformed", "parse error")):
        return "malformed"
    return "provider_unavailable"


def _cloud_payload(output: str) -> Any:
    try:
        return json.loads(output) if output.strip() else None
    except json.JSONDecodeError:
        return None


def _cloud_records(provider: str, kind: str, payload: Any, *, region: str = "", limit: int = _CLOUD_MAX_RECORDS) -> list[dict[str, Any]]:
    """Project provider-specific JSON into bounded, provider-neutral asset summaries."""
    candidates: list[Any] = []
    collection_keys = ("Response", "result", "Reservations", "Regions", "Region", "RegionSet", "regions", "value", "items", "Instances", "instances", "servers", "Instance", "InstanceSet", "instanceSet", "Buckets", "buckets", "Roles", "resources", "data")

    def collect(value: Any) -> None:
        if isinstance(value, list):
            for item in value:
                collect(item)
            return
        if not isinstance(value, Mapping):
            return
        for key in collection_keys:
            nested = value.get(key)
            if isinstance(nested, (Mapping, list)):
                collect(nested)
                return
        candidates.append(value)

    collect(payload)
    records: list[dict[str, Any]] = []
    for item in candidates[:max(1, min(_CLOUD_MAX_RECORDS, int(limit)))]:
        if not isinstance(item, Mapping):
            continue
        def first(*keys: str) -> str:
            for key in keys:
                value = item.get(key)
                if isinstance(value, Mapping):
                    value = value.get("Name") or value.get("name") or ""
                if isinstance(value, (str, int, float)) and not isinstance(value, bool) and value != "":
                    return str(value)
            return ""

        record = {
            "provider": provider,
            "kind": kind,
            "id": first("id", "Id", "InstanceId", "instance_id", "instanceId", "ResourceId", "resourceId", "Arn", "RoleId", "Name", "name", "RegionId", "RegionName", "Region", "region", "location"),
            "name": first("name", "Name", "InstanceName", "instance_name", "instanceName", "ResourceName", "resourceName", "RegionName", "DisplayName"),
            "region": first("region", "Region", "RegionName", "RegionId", "location", "Location") or region,
            "status": first("status", "Status", "State", "state", "PowerState", "instanceState"),
            "type": first("type", "Type", "ResourceType", "resourceType", "InstanceType", "instance_type", "instanceType"),
        }
        if record["id"]:
            records.append(record)
    return records


def _cloud_inventory_command(provider: str, kind: str, *, region: str) -> tuple[str, ...]:
    command = _CLOUD_INVENTORY_COMMANDS.get(provider, {}).get(kind, ())
    if not command:
        return ()
    args = list(command)
    if region and provider in {"aws", "tencent", "aliyun"}:
        args.extend(("--region", region))
    return tuple(args)


def _credential_type(credential_ref: Any) -> str:
    if not credential_ref:
        return "ambient"
    try:
        bundle = json.loads(str(credential_ref))
    except (TypeError, ValueError):
        return "opaque"
    if not isinstance(bundle, Mapping):
        return "opaque"
    keys = {str(key).casefold().replace("-", "_") for key in bundle}
    if keys & {"oidc_token", "web_identity_token", "federated_token"}:
        return "oidc"
    if keys & {"session_token", "security_token", "access_token", "token"}:
        return "temporary"
    if keys & {"profile", "sso_session"}:
        return "sso"
    if keys & {"role", "role_arn", "instance_role"}:
        return "instance_role"
    return "static"


def _cloud_identity(provider: str, payload: Any, *, region: str = "", credential_type: str = "cli") -> dict[str, str]:
    identity = {"account_id": "", "principal": "", "credential_type": credential_type, "region": region, "expires_at": "", "request_id": ""}
    if provider == "aws" and isinstance(payload, Mapping):
        identity.update(account_id=str(payload.get("Account") or ""), principal=str(payload.get("Arn") or ""))
    elif provider == "azure" and isinstance(payload, Mapping):
        user = payload.get("user") if isinstance(payload.get("user"), Mapping) else {}
        identity.update(
            account_id=str(payload.get("id") or ""),
            principal=str(user.get("name") or ""),
            credential_type=str(user.get("type") or "cli"),
            region=str(payload.get("location") or region),
        )
    elif provider == "gcp" and isinstance(payload, list):
        active = next((item for item in payload if isinstance(item, Mapping) and str(item.get("status") or "").upper() == "ACTIVE"), None)
        if active is not None:
            principal = str(active.get("account") or "")
            identity.update(
                account_id=str(active.get("project") or ""),
                principal=principal,
                credential_type="service_account" if "gserviceaccount.com" in principal else "user",
            )
    if isinstance(payload, Mapping):
        response = payload.get("Response") if isinstance(payload.get("Response"), Mapping) else payload
        identity["account_id"] = identity["account_id"] or str(
            response.get("AccountId") or response.get("AccountID") or response.get("ProjectId") or response.get("TenantId") or ""
        )
        identity["principal"] = identity["principal"] or str(
            response.get("Arn") or response.get("PrincipalId") or response.get("UserId") or response.get("UserName") or ""
        )
        identity["request_id"] = str(response.get("RequestId") or response.get("request_id") or "")
        identity["expires_at"] = str(response.get("Expiration") or response.get("ExpiresAt") or response.get("ExpirationTime") or "")
    return identity


def _cloud_identity_status(provider: str, payload: Any) -> str:
    if provider == "gcp":
        if not isinstance(payload, list):
            return "malformed"
        active = next((item for item in payload if isinstance(item, Mapping)
                       and str(item.get("status") or "").upper() == "ACTIVE"), None)
        if active is None:
            return "missing"
        return "valid" if isinstance(active.get("account"), str) and active["account"].strip() else "malformed"
    if not isinstance(payload, Mapping):
        return "malformed"
    response = payload.get("Response", payload)
    if not isinstance(response, Mapping):
        return "malformed"
    if provider == "aws":
        fields = (response.get("Account"), response.get("Arn"))
    elif provider == "azure":
        user = response.get("user")
        fields = (response.get("id"), user.get("name") if isinstance(user, Mapping) else None)
    else:
        fields = (response.get("AccountId") or response.get("AccountID"),
                  response.get("Arn") or response.get("PrincipalId") or response.get("UserId") or response.get("UserName"))
    return "valid" if all(isinstance(value, str) and value.strip() for value in fields) else "malformed"


def _cloud_environment(provider: str, credential_ref: Any) -> Mapping[str, str] | None:
    if credential_ref in (None, ""):
        return None
    try:
        bundle = json.loads(str(credential_ref))
    except json.JSONDecodeError:
        raise ValueError("credential_ref_malformed") from None
    if not isinstance(bundle, Mapping):
        raise ValueError("credential_ref_malformed")
    if provider == "azure":
        raise ValueError("credential_ref_azure_cli_login_required")
    if any(not isinstance(value, str) or "\x00" in value for value in bundle.values()):
        raise ValueError("credential_ref_malformed")
    normalized = {str(key).casefold().replace("-", "_"): str(value) for key, value in bundle.items() if value not in (None, "")}
    aliases = {
        "aws": {"access_key_id": "AWS_ACCESS_KEY_ID", "secret_access_key": "AWS_SECRET_ACCESS_KEY", "session_token": "AWS_SESSION_TOKEN", "profile": "AWS_PROFILE", "region": "AWS_DEFAULT_REGION"},
        "tencent": {"access_key_id": "TENCENTCLOUD_SECRET_ID", "secret_access_key": "TENCENTCLOUD_SECRET_KEY", "session_token": "TENCENTCLOUD_TOKEN", "region": "TENCENTCLOUD_REGION"},
        "aliyun": {"access_key_id": "ALIBABA_CLOUD_ACCESS_KEY_ID", "secret_access_key": "ALIBABA_CLOUD_ACCESS_KEY_SECRET", "session_token": "ALIBABA_CLOUD_SECURITY_TOKEN", "region": "ALIBABA_CLOUD_REGION_ID"},
        "azure": {"client_id": "AZURE_CLIENT_ID", "client_secret": "AZURE_CLIENT_SECRET", "tenant_id": "AZURE_TENANT_ID"},
        "gcp": {"access_token": "CLOUDSDK_AUTH_ACCESS_TOKEN", "project_id": "CLOUDSDK_CORE_PROJECT"},
    }
    provider_aliases = aliases.get(provider)
    if provider_aliases is None:
        raise ValueError("credential_ref_provider_unsupported")
    environment = os.environ.copy()
    if provider in {"aws", "tencent", "aliyun"}:
        if not (normalized.get("access_key_id") and normalized.get("secret_access_key")) and not (provider == "aws" and normalized.get("profile")):
            raise ValueError("credential_ref_incomplete")
        for key, env_name in provider_aliases.items():
            if key != "region":
                environment.pop(env_name, None)
        if provider == "aws":
            for env_name in ("AWS_SECURITY_TOKEN", "AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"):
                environment.pop(env_name, None)
            for env_name in tuple(environment):
                if env_name.startswith("AWS_ENDPOINT_URL"):
                    environment.pop(env_name, None)
            environment["AWS_IGNORE_CONFIGURED_ENDPOINT_URLS"] = "true"
            if normalized.get("access_key_id") and normalized.get("secret_access_key"):
                environment["AWS_CONFIG_FILE"] = os.devnull
                environment["AWS_SHARED_CREDENTIALS_FILE"] = os.devnull
    if provider == "gcp" and not normalized.get("access_token"):
        raise ValueError("credential_ref_incomplete")
    if provider == "gcp":
        for env_name in tuple(environment):
            if env_name.startswith(("CLOUDSDK_AUTH_", "CLOUDSDK_API_ENDPOINT_OVERRIDES_")):
                environment.pop(env_name, None)
    for key, env_name in provider_aliases.items():
        if normalized.get(key):
            environment[env_name] = normalized[key]
    if not any(normalized.get(key) for key in provider_aliases):
        raise ValueError("credential_ref_malformed")
    return environment


def _sdk_inventory(provider: str, arguments: Mapping[str, Any], base: Mapping[str, Any], *,
                   region: str, kinds: tuple[str, ...], explicit_kinds: bool, limit: int, timeout: float) -> dict[str, Any]:
    started = time.monotonic()
    result = {**base, **_cloud_identity(provider, None, region=region, credential_type=str(base["credential_type"])),
              "backend": "sdk", "sdk_package": cloud_sdk.SDK_PACKAGES[provider], "executable": "",
              "verification_source": "remote_read_probe", "identity_verified": False,
              "probe": "compute", "status": "missing"}
    bundle: dict[str, str] = {}
    try:
        bundle = cloud_sdk.credentials(provider, arguments.get("credential_ref"))
        region = region or bundle.get("region", "")
        if region and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", region):
            raise ValueError("credential_ref_region_invalid")
        result["region"] = region
        missing = [key for key in ("access_key_id", "secret_access_key") if not bundle.get(key)]
        if missing:
            result.update(status="credential_ref_invalid" if arguments.get("credential_ref") else "missing", missing_fields=missing)
            return result
        missing = [key for key, value in (("region", region), ("project_id", bundle.get("project_id") if provider == "huawei" else True)) if not value]
        if missing:
            result.update(status="configuration_required", missing_fields=missing)
            return result
        # JDCloud's pageSize range is 10..100. All four backends use one bounded page.
        page_size = max(10, min(limit, 100)) if provider == "jdcloud" else min(limit, 100)
        payload = cloud_sdk.compute(provider, bundle, region, page_size, timeout)
        error = payload.get("error")
        if error:
            diagnostic = json.dumps(error, default=str)
            result.update(status=_cloud_status(1, diagnostic, None), error=error)
            return result
        body = payload.get("result", payload)
        collection = "servers" if provider == "huawei" else "instances"
        if not isinstance(body, Mapping) or not isinstance(body.get(collection), list):
            result.update(status="malformed", error_message=f"response_missing_{collection}")
            return result
        result["status"] = "valid"
        result["request_id"] = str(payload.get("request_id") or payload.get("requestId") or "")
        if base["operation"] == "inventory":
            selected = kinds if explicit_kinds else ("compute",)
            failures = [{"kind": kind, "status": "unsupported"} for kind in selected if kind != "compute"]
            records = _cloud_records(provider, "compute", payload, region=region) if "compute" in selected else []
            total = body.get("total_count", body.get("totalCount", body.get("count")))
            truncated = bool(body.get("next_token") or body.get("next_marker") or body.get("nextMarker") or body.get("is_truncated") or
                             body.get("isTruncated") or len(records) > limit or
                             (isinstance(total, int) and total > len(records)) or
                             (total is None and len(records) >= page_size))
            assets = records[:limit]
            result.update(resource_types=list(selected), assets=assets, resource_count=len(assets), failures=failures,
                          truncated=truncated, inventory_complete=not failures and not truncated)
            if failures or truncated:
                result["status"] = "partial" if assets or truncated else "inventory_failed"
    except ImportError as error:
        result.update(status="sdk_backend_unavailable", missing_module=error.name)
    except ValueError as error:
        result.update(status="credential_ref_invalid" if str(error).startswith("credential_ref_") else "malformed", error_message=str(error))
    except Exception as error:
        diagnostic = str(error)
        for key in ("access_key_id", "secret_access_key", "session_token"):
            if bundle.get(key):
                diagnostic = diagnostic.replace(bundle[key], "[REDACTED]")
        result.update(status=_cloud_status(1, diagnostic, None), error_type=type(error).__name__, error_message=diagnostic)
    finally:
        result["latency_ms"] = round((time.monotonic() - started) * 1000, 3)
    return result


def cloud_inventory(arguments: Mapping[str, Any]) -> Mapping[str, Any]:
    requested = str(arguments.get("provider") or "auto").casefold().strip()
    requested = _CLOUD_ALIASES.get(requested, requested)
    operation = str(arguments.get("operation") or "inventory").casefold()
    if operation not in {"inventory", "credential_check", "permission_check"}:
        raise ValueError("cloud_operation_invalid")
    known_providers = tuple(_CLOUD_EXECUTABLES)
    if requested != "auto" and requested not in known_providers:
        raise ValueError("cloud_provider_invalid")
    providers = known_providers if requested == "auto" else (requested,)
    region = str(arguments.get("region") or "").strip()
    if region and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", region):
        raise ValueError("cloud_region_invalid")
    if requested == "auto" and arguments.get("credential_ref"):
        raise ValueError("cloud_credential_provider_required")
    explicit_kinds = "resource_types" in arguments or "scopes" in arguments
    raw_kinds = arguments.get("resource_types", arguments.get("scopes", _CLOUD_INVENTORY_DEFAULTS))
    if isinstance(raw_kinds, str):
        raw_kinds = (raw_kinds,)
    if not isinstance(raw_kinds, (list, tuple)) or len(raw_kinds) > 8 or any(not isinstance(item, str) or len(item) > 64 for item in raw_kinds):
        raise ValueError("cloud_resource_types_invalid")
    resource_types = tuple(dict.fromkeys(_CLOUD_INVENTORY_ALIASES.get(str(item).casefold().strip(), str(item).casefold().strip()) for item in raw_kinds if str(item).strip()))
    if operation == "inventory" and not resource_types:
        raise ValueError("cloud_resource_types_required")
    try:
        max_results = int(arguments.get("max_results", _CLOUD_MAX_RECORDS))
    except (TypeError, ValueError, OverflowError):
        raise ValueError("cloud_max_results_invalid") from None
    if type(arguments.get("max_results", _CLOUD_MAX_RECORDS)) is not int or not 1 <= max_results <= _CLOUD_MAX_RECORDS:
        raise ValueError("cloud_max_results_invalid")
    try:
        timeout = float(arguments.get("timeout", 30.0))
    except (TypeError, ValueError, OverflowError):
        raise ValueError("cloud_timeout_invalid") from None
    if isinstance(arguments.get("timeout"), bool) or not math.isfinite(timeout) or not 0.1 <= timeout <= 300:
        raise ValueError("cloud_timeout_invalid")
    checked: list[Mapping[str, Any]] = []
    for provider in providers:
        started = time.monotonic()
        executable = resolve_executable(_CLOUD_EXECUTABLES[provider])
        # auth list cannot identify or validate an explicitly supplied GCP token.
        remote_only = provider == "gcp" and bool(arguments.get("credential_ref"))
        command_map = _CLOUD_PERMISSION_COMMANDS if operation == "permission_check" or remote_only else _CLOUD_COMMANDS
        command = command_map.get(provider, ())
        credential_type = _credential_type(arguments.get("credential_ref"))
        base = {
            "provider": provider, "operation": operation, "credential_bound": bool(arguments.get("credential_ref")),
            "credential_type": credential_type, "evidence_type": f"cloud_{operation}", "secret_exposed": False,
        }
        if provider in cloud_sdk.SDK_PACKAGES:
            checked.append(_sdk_inventory(provider, arguments, base, region=region, kinds=resource_types,
                                          explicit_kinds=explicit_kinds, limit=max_results, timeout=timeout))
            continue
        if not executable:
            checked.append({**base, "status": "provider_unavailable", "executable": "", "latency_ms": 0.0, **_cloud_identity(provider, None, region=region, credential_type=credential_type)})
            continue
        try:
            environment = dict(_cloud_environment(provider, arguments.get("credential_ref")) or os.environ)
            environment["PYTHONDONTWRITEBYTECODE"] = "1"
            command_args = list(command[1:])
            if region and provider in {"aws", "tencent", "aliyun"}:
                command_args.extend(("--region", region))
            process = subprocess.run(
                (executable, *command_args),
                capture_output=True,
                text=True,
                errors="replace",
                timeout=timeout,
                check=False,
                env=environment,
            )
            diagnostic = process.stdout + "\n" + process.stderr
            payload = _cloud_payload(process.stdout)
            status = _cloud_status(process.returncode, diagnostic, payload)
            identity_payload = payload if command_map is _CLOUD_COMMANDS else None
            if status == "valid" and command_map is _CLOUD_COMMANDS:
                status = _cloud_identity_status(provider, payload)
                if status != "valid":
                    identity_payload = None
            # Local CLI identity caches do not prove remote credential acceptance.
            if status == "valid" and command_map is _CLOUD_COMMANDS and provider in {"azure", "gcp"}:
                probe = subprocess.run(
                    (executable, *_CLOUD_PERMISSION_COMMANDS[provider][1:]),
                    capture_output=True, text=True, errors="replace", timeout=timeout,
                    check=False, env=environment,
                )
                status = _cloud_status(probe.returncode, probe.stdout + "\n" + probe.stderr, _cloud_payload(probe.stdout))
                process = probe
                if status != "valid":
                    identity_payload = None
            result = {
                **base,
                "status": status,
                "executable": executable,
                "return_code": process.returncode,
                "verification_source": "remote_read_probe" if provider in {"azure", "gcp"} or operation == "permission_check" else "remote_identity",
                "identity_verified": status == "valid" and identity_payload is not None and provider not in {"azure", "gcp"},
                "latency_ms": round((time.monotonic() - started) * 1000, 3),
                "secret_exposed": False,
                **_cloud_identity(provider, identity_payload, region=region, credential_type=credential_type),
            }
            if operation == "inventory" and status == "valid":
                assets: list[dict[str, Any]] = []
                failures: list[dict[str, str]] = []
                truncated = False
                supported = _CLOUD_INVENTORY_COMMANDS.get(provider, {})
                kinds = resource_types if explicit_kinds else tuple(kind for kind in resource_types if kind in supported)
                for kind in kinds:
                    if kind not in supported:
                        failures.append({"kind": kind, "status": "unsupported"})
                        continue
                    inventory_command = _cloud_inventory_command(provider, kind, region=region)
                    try:
                        inventory_process = subprocess.run(
                            (executable, *inventory_command), capture_output=True, text=True, errors="replace",
                            timeout=timeout, check=False, env=environment,
                        )
                        inventory_payload = _cloud_payload(inventory_process.stdout)
                        inventory_status = _cloud_status(inventory_process.returncode, inventory_process.stdout + "\n" + inventory_process.stderr, inventory_payload)
                        if inventory_status == "valid":
                            records = _cloud_records(provider, kind, inventory_payload, region=region, limit=_CLOUD_MAX_RECORDS)
                            remaining = max_results - len(assets)
                            truncated |= len(records) >= _CLOUD_MAX_RECORDS or len(records) > remaining
                            response = inventory_payload.get("Response", inventory_payload) if isinstance(inventory_payload, Mapping) else {}
                            if isinstance(response, Mapping):
                                truncated |= any(response.get(key) for key in ("NextToken", "NextMarker", "Marker", "nextPageToken", "IsTruncated"))
                                total = response.get("TotalCount")
                                truncated |= isinstance(total, int) and total > len(records)
                            assets.extend(records[:remaining])
                        else:
                            failures.append({"kind": kind, "status": inventory_status})
                    except (OSError, subprocess.TimeoutExpired):
                        failures.append({"kind": kind, "status": "endpoint_unreachable"})
                result.update(resource_types=list(kinds), assets=assets, resource_count=len(assets), failures=failures, truncated=bool(truncated), inventory_complete=not failures and not truncated)
                if (failures or truncated) and assets:
                    result["status"] = "partial"
                elif failures and not assets:
                    result["status"] = "inventory_failed"
            checked.append(result)
        except ValueError:
            checked.append({
                **base,
                "status": "credential_ref_invalid",
                "executable": executable,
                "latency_ms": round((time.monotonic() - started) * 1000, 3),
                **_cloud_identity(provider, None, region=region, credential_type=credential_type),
            })
        except subprocess.TimeoutExpired:
            checked.append({
                **base,
                "status": "endpoint_unreachable",
                "executable": executable,
                "latency_ms": round((time.monotonic() - started) * 1000, 3),
                "secret_exposed": False,
                **_cloud_identity(provider, None, region=region, credential_type=credential_type),
            })
        except OSError:
            checked.append({
                **base,
                "status": "provider_unavailable",
                "executable": executable,
                "latency_ms": round((time.monotonic() - started) * 1000, 3),
                "secret_exposed": False,
                **_cloud_identity(provider, None, region=region, credential_type=credential_type),
            })
    secret_values = []
    if arguments.get("credential_ref"):
        try:
            bundle = json.loads(str(arguments["credential_ref"]))
            if isinstance(bundle, Mapping):
                secret_values = [value for key, value in bundle.items() if isinstance(value, str) and value and str(key).casefold().replace("-", "_") in {"access_key_id", "secret_access_key", "session_token", "access_token", "client_secret"}]
        except ValueError:
            pass
    else:
        for provider in providers:
            if provider in cloud_sdk.SDK_PACKAGES:
                bundle = cloud_sdk.credentials(provider, None)
                secret_values.extend(bundle[key] for key in ("access_key_id", "secret_access_key", "session_token") if bundle.get(key))
    # JDCloud's official SDK base64-encodes its temporary-token header.
    if "jdcloud" in providers:
        secret_values.extend(base64.b64encode(value.encode()).decode() for value in tuple(secret_values))

    def redact(value: Any) -> Any:
        if isinstance(value, str):
            for secret in secret_values:
                value = value.replace(secret, "[REDACTED]")
            return value
        if isinstance(value, dict):
            return {key: redact(item) for key, item in value.items()}
        if isinstance(value, list):
            return [redact(item) for item in value]
        return value

    return redact({
        "operation": operation,
        "requested_provider": requested,
        "providers": checked,
        "count": len(checked),
        "secret_exposed": False,
        "credential_bound": bool(arguments.get("credential_ref")),
    })
