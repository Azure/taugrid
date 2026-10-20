#!/usr/bin/env python3
import argparse
import base64
import hashlib
import http.client
import json
import os
import ssl
import subprocess
import tempfile
import urllib.parse

import strict_json


EXPECTED_CONTEXT = "aks-customer-004-eus-admin"
EXPECTED_CLUSTER = "aks-customer-004-eus"
EXPECTED_SERVER = (
    "https://aks-customer-004-eus-5czzjqga.hcp.eastus.azmk8s.io:443"
)
REQUEST_PATH = "/api/v1/nodes/gpu-07-03"
CONTENT_TYPE = "application/json-patch+json"
ACCEPT = "application/json"
USER_AGENT = "dapple-r050708-rev21-bootstrap/1"
MAX_RESPONSE_BYTES = 1024 * 1024
MAX_RESPONSE_HEADERS = 64
MAX_HEADER_NAME_BYTES = 128
MAX_HEADER_VALUE_BYTES = 8192


def canonical(value):
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def run_local(argv):
    completed = subprocess.run(
        argv,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(
            f"local command failed: {argv!r}: {completed.stderr.decode(errors='replace')}"
        )
    return completed.stdout


def exact_kubeconfig():
    context = run_local(["kubectl", "config", "current-context"]).decode().strip()
    if context != EXPECTED_CONTEXT:
        raise RuntimeError("current context mismatch")
    raw = run_local(
        ["kubectl", "config", "view", "--raw", "--minify", "-o", "json"]
    )
    value = strict_json.loads(raw, "minified kubeconfig", 1024 * 1024)
    if set(value) < {"clusters", "contexts", "current-context", "users"}:
        raise RuntimeError("minified kubeconfig shape mismatch")
    if value["current-context"] != EXPECTED_CONTEXT:
        raise RuntimeError("minified current context mismatch")
    if len(value["contexts"]) != 1 or len(value["clusters"]) != 1 or len(value["users"]) != 1:
        raise RuntimeError("minified kubeconfig cardinality mismatch")
    context_value = value["contexts"][0]
    cluster_value = value["clusters"][0]
    user_value = value["users"][0]
    if context_value["context"]["cluster"] != EXPECTED_CLUSTER:
        raise RuntimeError("context cluster mismatch")
    if cluster_value["name"] != EXPECTED_CLUSTER:
        raise RuntimeError("cluster name mismatch")
    if cluster_value["cluster"]["server"] != EXPECTED_SERVER:
        raise RuntimeError("cluster server mismatch")
    user = user_value["user"]
    required = {"client-certificate-data", "client-key-data"}
    if not required <= set(user):
        raise RuntimeError("rev17 requires client-certificate kubeconfig auth")
    forbidden = {"token", "tokenFile", "exec", "auth-provider", "username", "password"}
    if forbidden & set(user):
        raise RuntimeError("alternate or header-bearing auth is prohibited")
    cluster = cluster_value["cluster"]
    if "certificate-authority-data" not in cluster:
        raise RuntimeError("inline cluster CA data required")
    return {
        "server": cluster["server"],
        "ca": base64.b64decode(cluster["certificate-authority-data"]),
        "certificate": base64.b64decode(user["client-certificate-data"]),
        "key": base64.b64decode(user["client-key-data"]),
    }


def request_descriptor(patch):
    parsed = urllib.parse.urlsplit(EXPECTED_SERVER)
    body = canonical(patch)
    headers = [
        ["Host", parsed.netloc],
        ["Accept", ACCEPT],
        ["Content-Type", CONTENT_TYPE],
        ["Content-Length", str(len(body))],
        ["User-Agent", USER_AGENT],
        ["Connection", "close"],
    ]
    return {
        "method": "PATCH",
        "scheme": parsed.scheme,
        "authority": parsed.netloc,
        "path": REQUEST_PATH,
        "headers": headers,
        "bodyBase64": base64.b64encode(body).decode(),
        "bodySHA256": digest(body),
        "bodyBytes": len(body),
    }


def execute(patch, connection_factory=None, kubeconfig_loader=exact_kubeconfig):
    request = request_descriptor(patch)
    directory = None
    result = None
    try:
        if connection_factory is None:
            config = kubeconfig_loader()
            parsed = urllib.parse.urlsplit(config["server"])
            directory = tempfile.TemporaryDirectory(prefix="rev20-kube-auth-")
            ca_path = os.path.join(directory.name, "ca.crt")
            cert_path = os.path.join(directory.name, "client.crt")
            key_path = os.path.join(directory.name, "client.key")
            for path, data in (
                (ca_path, config["ca"]),
                (cert_path, config["certificate"]),
                (key_path, config["key"]),
            ):
                descriptor = os.open(
                    path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
                )
                with os.fdopen(descriptor, "wb") as output:
                    output.write(data)
            context = ssl.create_default_context(cafile=ca_path)
            context.load_cert_chain(certfile=cert_path, keyfile=key_path)
            connection = http.client.HTTPSConnection(
                parsed.hostname,
                parsed.port or 443,
                context=context,
                timeout=30,
            )
        else:
            connection = connection_factory()
        result = _exchange(connection, request)
    except Exception as error:
        result = _transport_failure(request, error)
    finally:
        if directory is not None:
            try:
                directory.cleanup()
            except Exception as cleanup_error:
                if result is None or result["transport"]["completed"] is True:
                    result = _transport_failure(request, cleanup_error)
    return result


def _safe_transport_error(error):
    import socket

    if isinstance(error, (TimeoutError, socket.timeout)):
        return "TIMEOUT", "transport timed out", {}
    if isinstance(error, ssl.SSLCertVerificationError):
        return "TLS_CERTIFICATE_VERIFICATION_FAILED", "TLS certificate verification failed", {}
    if isinstance(error, ssl.SSLError):
        return "TLS_FAILURE", "TLS transport failed", {}
    if isinstance(error, ConnectionRefusedError):
        return "CONNECTION_REFUSED", "connection refused", {}
    if isinstance(error, (ConnectionResetError, BrokenPipeError)):
        return "CONNECTION_RESET", "connection reset", {}
    if isinstance(error, http.client.HTTPException):
        return "HTTP_PROTOCOL_ERROR", "HTTP protocol failed", {}
    if isinstance(error, OSError):
        metadata = {}
        if type(error.errno) is int:
            metadata["errno"] = error.errno
        return "NETWORK_OS_ERROR", "network operating-system error", metadata
    return "UNCLASSIFIED_TRANSPORT_ERROR", "unclassified transport failure", {}


def _bounded_headers(headers):
    result = []
    complete = True
    for index, (name, value) in enumerate(headers):
        if index >= MAX_RESPONSE_HEADERS:
            complete = False
            break
        name_raw = name.encode("utf-8", errors="strict")
        value_raw = value.encode("utf-8", errors="strict")
        if (
            len(name_raw) > MAX_HEADER_NAME_BYTES
            or len(value_raw) > MAX_HEADER_VALUE_BYTES
        ):
            complete = False
            break
        result.append([name, value])
    return result, complete


def _bounded_reason(reason):
    if type(reason) is not str:
        raise ValueError("HTTP reason type mismatch")
    raw = reason.encode("ascii", errors="strict")
    if len(raw) > 256 or any(byte < 32 or byte > 126 for byte in raw):
        raise ValueError("HTTP reason syntax or bounds mismatch")
    return reason


def _http_version(version):
    versions = {10: "HTTP/1.0", 11: "HTTP/1.1"}
    if version not in versions:
        raise ValueError("HTTP version mismatch")
    return versions[version]


def _http_status(status):
    if type(status) is not int or not 100 <= status <= 599:
        raise ValueError("HTTP status mismatch")
    return status


def _json_native(value):
    if value is None or type(value) in (bool, int, str):
        return value
    if type(value) is list:
        return [_json_native(item) for item in value]
    if type(value) is dict:
        return {key: _json_native(item) for key, item in value.items()}
    raise ValueError("response projection contains non-JSON-native number")


def _transport_failure(request, error):
    code, message, metadata = _safe_transport_error(error)
    return {
        "schema": "dapple-r050708-rev21-wire-exchange/v1",
        "request": request,
        "transport": {
            "completed": False,
            "errorCode": code,
            "redactedMessage": message,
            "safeMetadata": metadata,
        },
        "response": None,
    }


def _projection(status, body):
    try:
        value = strict_json.loads(body, "HTTP response body", MAX_RESPONSE_BYTES)
    except strict_json.StrictJSONError:
        return None, "STRICT_JSON_REJECTED"
    if 200 <= status < 300:
        if type(value) is not dict:
            return None, "SUCCESS_OBJECT_TYPE_REJECTED"
        if set(value) != {"apiVersion", "kind", "metadata", "spec", "status"}:
            return None, "SUCCESS_OBJECT_KEYS_REJECTED"
        metadata = value.get("metadata")
        if type(metadata) is not dict:
            return None, "SUCCESS_METADATA_TYPE_REJECTED"
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
            return None, "SUCCESS_METADATA_REQUIRED_KEYS_REJECTED"
        if not set(metadata) <= allowed_metadata:
            return None, "SUCCESS_METADATA_KEYS_REJECTED"
        projection = {
            "apiVersion": value.get("apiVersion"),
            "kind": value.get("kind"),
            "metadata": {
                "name": metadata.get("name"),
                "uid": metadata.get("uid"),
                "resourceVersion": metadata.get("resourceVersion"),
            },
            "spec": value.get("spec"),
        }
        try:
            return _json_native(projection), None
        except ValueError:
            return None, "NON_JSON_NATIVE_NUMBER_REJECTED"
    try:
        return _json_native(value), None
    except ValueError:
        return None, "NON_JSON_NATIVE_NUMBER_REJECTED"


def _exchange(connection, request):
    body = base64.b64decode(request["bodyBase64"])
    result = None
    operation_error = None
    try:
        connection.putrequest(
            request["method"],
            request["path"],
            skip_host=True,
            skip_accept_encoding=True,
        )
        for name, value in request["headers"]:
            connection.putheader(name, value)
        connection.endheaders(body)
        response = connection.getresponse()
        status = _http_status(response.status)
        reason = _bounded_reason(response.reason)
        http_version = _http_version(response.version)
        response_body = response.read(MAX_RESPONSE_BYTES + 1)
        truncated = len(response_body) > MAX_RESPONSE_BYTES
        if truncated:
            response_body = response_body[:MAX_RESPONSE_BYTES]
        response_headers, headers_complete = _bounded_headers(
            response.getheaders()
        )
        projection, projection_error = _projection(
            status, response_body
        )
        result = {
            "schema": "dapple-r050708-rev21-wire-exchange/v1",
            "request": request,
            "transport": {
                "completed": True,
                "errorType": None,
                "errorMessage": None,
            },
            "response": {
                "httpVersion": http_version,
                "status": status,
                "reason": reason,
                "statusLine": f"{http_version} {status} {reason}",
                "headers": response_headers,
                "headersComplete": headers_complete,
                "bodyBase64": base64.b64encode(response_body).decode(),
                "bodySHA256": digest(response_body),
                "bodyBytes": len(response_body),
                "bodyComplete": not truncated,
                "bodyTruncated": truncated,
                "decodedProjection": projection,
                "projectionErrorCode": projection_error,
            },
        }
    except Exception as error:
        operation_error = error
        result = _transport_failure(request, error)
    try:
        connection.close()
    except Exception as close_error:
        if operation_error is None:
            result = _transport_failure(request, close_error)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--patch-base64", required=True)
    arguments = parser.parse_args()
    patch_raw = base64.b64decode(arguments.patch_base64)
    patch = strict_json.loads(patch_raw, "patch", 1024 * 1024)
    if patch_raw != canonical(patch):
        raise RuntimeError("patch bytes are not canonical")
    print(canonical(execute(patch)).decode(), end="")


if __name__ == "__main__":
    main()
