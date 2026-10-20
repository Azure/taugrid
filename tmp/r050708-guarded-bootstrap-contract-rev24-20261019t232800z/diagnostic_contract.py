import base64
import copy
import hashlib
import json
import re

import strict_json


NODE_UID = "d882c2c0-17e3-4486-aab5-139c71768ce3"
OLD_TRANSFER_ID = "d8976fe9-bf74-432b-85a2-e887e22d57e9"
SOURCE_RESERVATION = "rows05-08-remediation-20261019t003520z-v2"
TARGET_RESERVATION = "rows05-08-remediation-20261019t080808z-v3"
REQUEST_PATH = "/api/v1/nodes/gpu-07-03"
CONTENT_TYPE = "application/json-patch+json"
EXPECTED_AUTHORITY = "aks-customer-004-eus-5czzjqga.hcp.eastus.azmk8s.io:443"
USER_AGENT = "dapple-r050708-rev21-bootstrap/1"
MAX_RESPONSE_HEADERS = 64
PREDECESSOR_SPEC = {
    "taints": [
        {"effect": "NoSchedule", "key": "nvidia.com/gpu", "value": "present"},
        {
            "effect": "NoSchedule",
            "key": "project-signal.azure.com/certification-reserved",
            "value": SOURCE_RESERVATION,
        },
        {
            "effect": "NoSchedule",
            "key": "node.kubernetes.io/unschedulable",
            "timeAdded": "2026-10-19T07:53:55Z",
        },
    ],
    "unschedulable": True,
}
TARGET_SPEC = copy.deepcopy(PREDECESSOR_SPEC)
TARGET_SPEC["taints"][1]["value"] = TARGET_RESERVATION


class ContractError(ValueError):
    pass


def canonical(value):
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def exact_keys(value, keys, label):
    if type(value) is not dict or set(value) != set(keys):
        raise ContractError(f"{label} keys mismatch")


def build_patch(resource_version):
    if type(resource_version) is not str or not resource_version:
        raise ContractError("fresh resourceVersion required")
    return [
        {"op": "test", "path": "/metadata/uid", "value": NODE_UID},
        {
            "op": "test",
            "path": "/metadata/resourceVersion",
            "value": resource_version,
        },
        {"op": "test", "path": "/spec", "value": copy.deepcopy(PREDECESSOR_SPEC)},
        {"op": "replace", "path": "/spec", "value": copy.deepcopy(TARGET_SPEC)},
    ]


def validate_patch(patch, resource_version):
    if patch != build_patch(resource_version):
        raise ContractError("guarded patch mismatch")


def header_map(headers):
    if type(headers) is not list:
        raise ContractError("headers must be ordered list")
    if len(headers) > MAX_RESPONSE_HEADERS:
        raise ContractError("header count exceeds ceiling")
    result = {}
    for item in headers:
        if type(item) is not list or len(item) != 2:
            raise ContractError("header tuple mismatch")
        name, value = item
        if (
            type(name) is not str
            or type(value) is not str
            or not re.fullmatch(r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+", name)
            or len(name.encode()) > 128
            or len(value.encode()) > 8192
            or any(ord(character) < 32 and character != "\t" for character in value)
        ):
            raise ContractError("header syntax or bounds mismatch")
        lowered = name.lower()
        if lowered in result:
            raise ContractError("duplicate header")
        result[lowered] = value
    return result


def validate_request(request, patch):
    exact_keys(
        request,
        {
            "method",
            "scheme",
            "authority",
            "path",
            "headers",
            "bodyBase64",
            "bodySHA256",
            "bodyBytes",
        },
        "request",
    )
    body = base64.b64decode(request["bodyBase64"], validate=True)
    if body != canonical(patch):
        raise ContractError("wire body mismatch")
    if request["bodySHA256"] != digest(body) or request["bodyBytes"] != len(body):
        raise ContractError("wire body descriptor mismatch")
    if request["method"] != "PATCH" or request["path"] != REQUEST_PATH:
        raise ContractError("wire method or path mismatch")
    if request["scheme"] != "https":
        raise ContractError("wire scheme mismatch")
    if request["authority"] != EXPECTED_AUTHORITY:
        raise ContractError("wire authority mismatch")
    expected_headers = [
        ["Host", EXPECTED_AUTHORITY],
        ["Accept", "application/json"],
        ["Content-Type", CONTENT_TYPE],
        ["Content-Length", str(len(body))],
        ["User-Agent", USER_AGENT],
        ["Connection", "close"],
    ]
    if request["headers"] != expected_headers:
        raise ContractError("wire ordered headers mismatch")
    headers = header_map(request["headers"])
    expected = {
        "host",
        "accept",
        "content-type",
        "content-length",
        "user-agent",
        "connection",
    }
    if set(headers) != expected:
        raise ContractError("wire headers mismatch")
    if headers["content-type"] != CONTENT_TYPE:
        raise ContractError("wire Content-Type mismatch")
    if headers["accept"] != "application/json":
        raise ContractError("wire Accept mismatch")
    if headers["content-length"] != str(len(body)):
        raise ContractError("wire Content-Length mismatch")
    return body


def response_content_type(headers):
    values = header_map(headers)
    value = values.get("content-type")
    if value is None:
        raise ContractError("response Content-Type missing")
    return value.split(";", 1)[0].strip().lower()


def validate_status(value, status):
    allowed = {
        "apiVersion",
        "kind",
        "metadata",
        "status",
        "message",
        "reason",
        "code",
    }
    if type(value) is not dict or set(value) not in (allowed, allowed | {"details"}):
        raise ContractError("Kubernetes Status keys mismatch")
    if (
        value["apiVersion"] != "v1"
        or value["kind"] != "Status"
        or value["status"] != "Failure"
        or value["code"] != status
        or type(value["metadata"]) is not dict
        or value["metadata"] != {}
        or type(value["message"]) is not str
        or not value["message"]
        or type(value["reason"]) is not str
        or not value["reason"]
    ):
        raise ContractError("Kubernetes Status mismatch")
    _safe_status_text(value["reason"], "reason", 128)
    _safe_status_text(value["message"], "message", 4096)
    if value["reason"] not in {
        "BadRequest",
        "Conflict",
        "Forbidden",
        "Unauthorized",
        "Invalid",
        "NotFound",
        "MethodNotAllowed",
        "InternalError",
        "ServerTimeout",
        "Timeout",
        "TooManyRequests",
        "ServiceUnavailable",
    }:
        raise ContractError("Status reason not allowlisted")
    if "details" in value:
        _validate_status_details(value["details"])
    return value


def _safe_status_text(value, label, ceiling):
    if len(value.encode("utf-8")) > ceiling:
        raise ContractError(f"Status {label} too large")
    if any(ord(character) < 32 and character not in "\t\n" for character in value):
        raise ContractError(f"Status {label} contains control character")


def _validate_status_details(value):
    if type(value) is not dict:
        raise ContractError("Status details type mismatch")
    allowed = {
        "name",
        "group",
        "kind",
        "uid",
        "causes",
        "retryAfterSeconds",
    }
    if not set(value) <= allowed:
        raise ContractError("Status details keys mismatch")
    for name in ("name", "group", "kind", "uid"):
        if name in value:
            if type(value[name]) is not str:
                raise ContractError(f"Status details {name} type mismatch")
            _safe_status_text(value[name], f"details.{name}", 512)
    if "retryAfterSeconds" in value and (
        type(value["retryAfterSeconds"]) is not int
        or value["retryAfterSeconds"] < 0
    ):
        raise ContractError("Status retryAfterSeconds mismatch")
    if "causes" in value:
        if type(value["causes"]) is not list or len(value["causes"]) > 64:
            raise ContractError("Status causes mismatch")
        for cause in value["causes"]:
            exact_keys(cause, {"reason", "message", "field"}, "Status cause")
            for name in ("reason", "message", "field"):
                if type(cause[name]) is not str:
                    raise ContractError("Status cause type mismatch")
                _safe_status_text(cause[name], f"cause.{name}", 1024)


def validate_success_projection(value, request_resource_version):
    exact_keys(value, {"apiVersion", "kind", "metadata", "spec"}, "success projection")
    exact_keys(value["metadata"], {"name", "uid", "resourceVersion"}, "success metadata")
    if (
        value["apiVersion"] != "v1"
        or value["kind"] != "Node"
        or value["metadata"]["name"] != "gpu-07-03"
        or value["metadata"]["uid"] != NODE_UID
        or value["spec"] != TARGET_SPEC
    ):
        raise ContractError("success projection mismatch")
    rv = value["metadata"]["resourceVersion"]
    if type(rv) is not str or not rv or rv == request_resource_version:
        raise ContractError("success resourceVersion mismatch")
    return rv


def project_success(value):
    if type(value) is not dict:
        raise ContractError("success object type mismatch")
    exact_keys(
        value,
        {"apiVersion", "kind", "metadata", "spec", "status"},
        "success object",
    )
    metadata = value["metadata"]
    if type(metadata) is not dict:
        raise ContractError("success metadata type mismatch")
    allowed_metadata = {
        "name",
        "generateName",
        "namespace",
        "selfLink",
        "uid",
        "resourceVersion",
        "generation",
        "creationTimestamp",
        "deletionTimestamp",
        "deletionGracePeriodSeconds",
        "labels",
        "annotations",
        "ownerReferences",
        "finalizers",
        "managedFields",
        "clusterName",
    }
    if not {"name", "uid", "resourceVersion"} <= set(metadata):
        raise ContractError("success metadata required keys missing")
    if not set(metadata) <= allowed_metadata:
        raise ContractError("success metadata keys mismatch")
    if type(value["status"]) is not dict:
        raise ContractError("success status type mismatch")
    return {
        "apiVersion": value["apiVersion"],
        "kind": value["kind"],
        "metadata": {
            "name": metadata["name"],
            "uid": metadata["uid"],
            "resourceVersion": metadata["resourceVersion"],
        },
        "spec": value["spec"],
    }


def classify_exchange(exchange, patch, request_resource_version):
    exact_keys(exchange, {"schema", "request", "transport", "response"}, "exchange")
    if exchange["schema"] != "dapple-r050708-rev21-wire-exchange/v1":
        raise ContractError("exchange schema mismatch")
    validate_patch(patch, request_resource_version)
    validate_request(exchange["request"], patch)
    transport = exchange["transport"]
    if type(transport) is not dict or "completed" not in transport:
        raise ContractError("transport shape mismatch")
    if transport["completed"] is False:
        exact_keys(
            transport,
            {"completed", "errorCode", "redactedMessage", "safeMetadata"},
            "transport failure",
        )
        if exchange["response"] is not None:
            raise ContractError("transport failure cannot have response")
        allowed = {
            "TIMEOUT": "transport timed out",
            "TLS_CERTIFICATE_VERIFICATION_FAILED": "TLS certificate verification failed",
            "TLS_FAILURE": "TLS transport failed",
            "CONNECTION_REFUSED": "connection refused",
            "CONNECTION_RESET": "connection reset",
            "HTTP_PROTOCOL_ERROR": "HTTP protocol failed",
            "NETWORK_OS_ERROR": "network operating-system error",
            "UNCLASSIFIED_TRANSPORT_ERROR": "unclassified transport failure",
        }
        if allowed.get(transport["errorCode"]) != transport["redactedMessage"]:
            raise ContractError("transport error is not allowlisted and redacted")
        if type(transport["safeMetadata"]) is not dict or set(
            transport["safeMetadata"]
        ) - {"errno"}:
            raise ContractError("transport safe metadata mismatch")
        if "errno" in transport["safeMetadata"] and type(
            transport["safeMetadata"]["errno"]
        ) is not int:
            raise ContractError("transport errno type mismatch")
        return {"outcome": "FAIL_CLOSED_TRANSPORT", "status": None}
    if transport != {"completed": True, "errorType": None, "errorMessage": None}:
        raise ContractError("completed transport shape mismatch")
    response = exchange["response"]
    exact_keys(
        response,
        {
            "httpVersion",
            "status",
            "reason",
            "statusLine",
            "headers",
            "headersComplete",
            "bodyBase64",
            "bodySHA256",
            "bodyBytes",
            "bodyComplete",
            "bodyTruncated",
            "decodedProjection",
            "projectionErrorCode",
        },
        "response",
    )
    if (
        response["headersComplete"] is not True
        or response["bodyComplete"] is not True
        or response["bodyTruncated"] is not False
    ):
        raise ContractError("response body incomplete or truncated")
    body = base64.b64decode(response["bodyBase64"], validate=True)
    if response["bodySHA256"] != digest(body) or response["bodyBytes"] != len(body):
        raise ContractError("response body descriptor mismatch")
    if not body:
        raise ContractError("response body missing")
    content_type = response_content_type(response["headers"])
    if content_type != "application/json":
        raise ContractError("response Content-Type is not JSON")
    try:
        value = strict_json.loads(body, "HTTP response body", 1024 * 1024)
    except strict_json.StrictJSONError as error:
        raise ContractError("response body is not strict JSON") from error
    status = response["status"]
    if type(status) is not int or not 100 <= status <= 599:
        raise ContractError("HTTP status type mismatch")
    reason = response["reason"]
    if (
        type(reason) is not str
        or len(reason.encode("ascii", errors="ignore")) != len(reason)
        or len(reason.encode("ascii")) > 256
        or any(ord(character) < 32 or ord(character) > 126 for character in reason)
    ):
        raise ContractError("HTTP reason syntax or bounds mismatch")
    if response["httpVersion"] not in {"HTTP/1.0", "HTTP/1.1"}:
        raise ContractError("HTTP version mismatch")
    if response["statusLine"] != f'{response["httpVersion"]} {status} {reason}':
        raise ContractError("HTTP status line mismatch")
    if 200 <= status < 300:
        expected_projection = project_success(value)
        if (
            response["projectionErrorCode"] is not None
            or response["decodedProjection"] != expected_projection
        ):
            raise ContractError("success projection evidence mismatch")
        rv = validate_success_projection(
            response["decodedProjection"], request_resource_version
        )
        return {"outcome": "SUCCESS", "status": status, "resourceVersion": rv}
    if (
        response["projectionErrorCode"] is not None
        or response["decodedProjection"] != value
    ):
        raise ContractError("Status projection evidence mismatch")
    status_value = validate_status(value, status)
    reason = status_value["reason"]
    if status == 409 or reason == "Conflict":
        outcome = "FAIL_CLOSED_JSON_PATCH_CONFLICT"
    elif status == 422 or reason == "Invalid":
        outcome = "FAIL_CLOSED_SCHEMA_OR_VALIDATION_DENIAL"
    elif status in (401, 403) or reason in ("Forbidden", "Unauthorized"):
        outcome = "FAIL_CLOSED_ADMISSION_OR_AUTHORIZATION_DENIAL"
    elif status == 400 or reason == "BadRequest":
        outcome = "FAIL_CLOSED_BAD_REQUEST"
    else:
        outcome = "FAIL_CLOSED_OTHER_API_STATUS"
    return {
        "outcome": outcome,
        "status": status,
        "reason": reason,
        "message": status_value["message"],
    }


def validate_execution(value, new_transfer_id):
    exact_keys(
        value,
        {
            "transferID",
            "transferStateBefore",
            "transferConsumed",
            "calls",
            "retryCount",
            "fallbackCount",
            "postStateGetCount",
            "extraCallCount",
        },
        "execution",
    )
    if new_transfer_id == OLD_TRANSFER_ID or value["transferID"] != new_transfer_id:
        raise ContractError("consumed transfer reuse")
    if value["transferStateBefore"] != "ACCEPTED_UNCONSUMED":
        raise ContractError("transfer pre-state mismatch")
    if value["transferConsumed"] is not True:
        raise ContractError("transfer consumption missing")
    if value["calls"] != ["compact-near-write-get", "raw-diagnostic-patch"]:
        raise ContractError("exact two-call sequence mismatch")
    if (
        value["retryCount"] != 0
        or value["fallbackCount"] != 0
        or value["postStateGetCount"] != 0
        or value["extraCallCount"] != 0
    ):
        raise ContractError("retry, fallback, post-state GET, or extra call detected")
