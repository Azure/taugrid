import base64
import copy
import json
import math
import ssl
import unittest

import diagnostic_contract as contract
import raw_patch_client as client
import strict_json


RV = "30000001"


class FakeResponse:
    def __init__(self, status, headers, body, reason="OK", version=11):
        self.status = status
        self.reason = reason
        self.version = version
        self._headers = headers
        self._body = body

    def getheaders(self):
        return self._headers

    def read(self, size):
        return self._body[:size]


class FakeConnection:
    def __init__(self, response=None, error=None, close_error=None):
        self.response = response
        self.error = error
        self.close_error = close_error

    def putrequest(self, method, path, **kwargs):
        pass

    def putheader(self, name, value):
        pass

    def endheaders(self, body):
        if self.error:
            raise self.error

    def getresponse(self):
        return self.response

    def close(self):
        if self.close_error:
            raise self.close_error


def status_body(overrides=None):
    value = {
        "apiVersion": "v1",
        "kind": "Status",
        "metadata": {},
        "status": "Failure",
        "message": "test failed",
        "reason": "Conflict",
        "code": 409,
    }
    if overrides:
        value.update(overrides)
    return contract.canonical(value)


def success_body(overrides=None):
    value = {
        "apiVersion": "v1",
        "kind": "Node",
        "metadata": {
            "name": "gpu-07-03",
            "uid": contract.NODE_UID,
            "resourceVersion": "30000002",
        },
        "spec": copy.deepcopy(contract.TARGET_SPEC),
        "status": {},
    }
    if overrides:
        value.update(overrides)
    return contract.canonical(value)


def exchange(status, body, headers=None):
    response = FakeResponse(
        status,
        headers or [("Content-Type", "application/json")],
        body,
    )
    return client.execute(
        contract.build_patch(RV),
        connection_factory=lambda: FakeConnection(response=response),
    )


class Rev18SecurityTests(unittest.TestCase):
    def assert_rejected(self, value):
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(value, contract.build_patch(RV), RV)

    def test_duplicate_status_top_level_rejected(self):
        body = (
            b'{"apiVersion":"v1","kind":"Status","metadata":{},'
            b'"status":"Failure","message":"a","message":"b",'
            b'"reason":"Conflict","code":409}'
        )
        self.assert_rejected(exchange(409, body))

    def test_duplicate_status_nested_details_rejected(self):
        body = (
            b'{"apiVersion":"v1","kind":"Status","metadata":{},'
            b'"status":"Failure","message":"a","reason":"Invalid","code":422,'
            b'"details":{"name":"a","name":"b"}}'
        )
        self.assert_rejected(exchange(422, body))

    def test_duplicate_success_identity_rejected(self):
        body = (
            b'{"apiVersion":"v1","kind":"Node","metadata":'
            b'{"name":"gpu-07-03","uid":"'
            + contract.NODE_UID.encode()
            + b'","uid":"other","resourceVersion":"30000002"},'
            b'"spec":'
            + contract.canonical(contract.TARGET_SPEC)
            + b',"status":{}}'
        )
        self.assert_rejected(exchange(200, body))

    def test_duplicate_success_spec_rejected(self):
        body = (
            b'{"apiVersion":"v1","kind":"Node","metadata":'
            b'{"name":"gpu-07-03","uid":"'
            + contract.NODE_UID.encode()
            + b'","resourceVersion":"30000002"},'
            b'"spec":'
            + contract.canonical(contract.TARGET_SPEC)
            + b',"spec":{},"status":{}}'
        )
        self.assert_rejected(exchange(200, body))

    def test_duplicate_success_resource_version_rejected(self):
        body = (
            b'{"apiVersion":"v1","kind":"Node","metadata":'
            b'{"name":"gpu-07-03","uid":"'
            + contract.NODE_UID.encode()
            + b'","resourceVersion":"30000002","resourceVersion":"30000003"},'
            b'"spec":'
            + contract.canonical(contract.TARGET_SPEC)
            + b',"status":{}}'
        )
        self.assert_rejected(exchange(200, body))

    def test_non_finite_numbers_rejected(self):
        for constant in (b"NaN", b"Infinity", b"-Infinity"):
            body = status_body().replace(b'"code":409', b'"code":' + constant)
            self.assert_rejected(exchange(409, body))

    def test_exponent_overflow_is_finite_preserving(self):
        value = strict_json.loads(b'{"value":1e9999,"small":1e-9999,"negative":-0.0}')
        self.assertTrue(value["value"].is_finite())
        self.assertEqual(str(value["value"]), "1E+9999")
        self.assertEqual(str(value["small"]), "1E-9999")
        self.assertTrue(value["negative"].is_signed())
        body = success_body().replace(
            b'"resourceVersion":"30000002"',
            b'"generation":1e9999,"resourceVersion":"30000002"',
        )
        result = contract.classify_exchange(exchange(200, body), contract.build_patch(RV), RV)
        self.assertEqual(result["outcome"], "SUCCESS")

    def test_large_integer_remains_exact(self):
        value = strict_json.loads(b'{"value":123456789012345678901234567890}')
        self.assertEqual(value["value"], 123456789012345678901234567890)

    def test_extra_status_field_rejected(self):
        self.assert_rejected(
            exchange(409, status_body({"unexpected": "value"}))
        )

    def test_missing_status_field_rejected(self):
        value = json.loads(status_body())
        del value["reason"]
        self.assert_rejected(exchange(409, contract.canonical(value)))

    def test_wrong_status_types_rejected(self):
        for overrides in (
            {"metadata": []},
            {"message": 1},
            {"reason": 1},
            {"code": "409"},
        ):
            self.assert_rejected(exchange(409, status_body(overrides)))

    def test_extra_success_projection_field_rejected(self):
        value = exchange(200, success_body())
        value["response"]["decodedProjection"]["extra"] = True
        self.assert_rejected(value)

    def test_extra_success_raw_field_rejected(self):
        self.assert_rejected(
            exchange(200, success_body({"unexpected": "value"}))
        )

    def test_extra_success_metadata_field_rejected(self):
        value = json.loads(success_body())
        value["metadata"]["unexpected"] = "value"
        self.assert_rejected(exchange(200, contract.canonical(value)))

    def test_missing_success_projection_field_rejected(self):
        value = exchange(200, success_body())
        del value["response"]["decodedProjection"]["spec"]
        self.assert_rejected(value)

    def test_wrong_success_types_rejected(self):
        for overrides in (
            {"apiVersion": 1},
            {"kind": 1},
            {"metadata": []},
            {"spec": []},
        ):
            self.assert_rejected(exchange(200, success_body(overrides)))

    def test_malicious_status_text_rejected(self):
        values = [
            "bad\x00control",
            "x" * 5000,
        ]
        for value in values:
            self.assert_rejected(exchange(409, status_body({"message": value})))
        self.assert_rejected(
            exchange(409, status_body({"reason": "MaliciousReason"}))
        )

    def test_malicious_status_details_rejected(self):
        values = [
            {"secret": "token"},
            {"name": 1},
            {
                "causes": [
                    {
                        "reason": "x",
                        "message": "bad\x00control",
                        "field": "spec",
                    }
                ]
            },
        ]
        for details in values:
            self.assert_rejected(exchange(409, status_body({"details": details})))

    def test_private_material_never_sealed_from_exception(self):
        secrets = [
            "-----BEGIN PRIVATE KEY-----abc",
            "-----BEGIN CERTIFICATE-----abc",
            "Bearer token-value",
            "Authorization: Basic abc",
            "Cookie: session=abc",
            "client-key-data: abc",
            "?token=abc&body=secret",
            "unicode-\u2603-\x00-" + "x" * 20000,
        ]
        for secret in secrets:
            value = client.execute(
                contract.build_patch(RV),
                connection_factory=lambda secret=secret: FakeConnection(
                    error=RuntimeError(secret)
                ),
            )
            raw = contract.canonical(value)
            self.assertNotIn(secret.encode(errors="ignore"), raw)
            result = contract.classify_exchange(
                value, contract.build_patch(RV), RV
            )
            self.assertEqual(result["outcome"], "FAIL_CLOSED_TRANSPORT")

    def test_allowlisted_tls_error(self):
        value = client.execute(
            contract.build_patch(RV),
            connection_factory=lambda: FakeConnection(
                error=ssl.SSLError("-----BEGIN PRIVATE KEY-----")
            ),
        )
        self.assertEqual(value["transport"]["errorCode"], "TLS_FAILURE")
        self.assertNotIn("PRIVATE KEY", contract.canonical(value).decode())

    def test_close_exception_is_redacted(self):
        secret = "-----BEGIN PRIVATE KEY-----CLOSE-SECRET"
        response = FakeResponse(
            200, [("Content-Type", "application/json")], success_body()
        )
        value = client.execute(
            contract.build_patch(RV),
            connection_factory=lambda: FakeConnection(
                response=response, close_error=RuntimeError(secret)
            ),
        )
        self.assertNotIn(secret, contract.canonical(value).decode())
        self.assertEqual(
            contract.classify_exchange(value, contract.build_patch(RV), RV)["outcome"],
            "FAIL_CLOSED_TRANSPORT",
        )

    def test_primary_error_preserved_when_close_also_fails(self):
        value = client.execute(
            contract.build_patch(RV),
            connection_factory=lambda: FakeConnection(
                error=TimeoutError("primary-secret"),
                close_error=RuntimeError("close-secret"),
            ),
        )
        self.assertEqual(value["transport"]["errorCode"], "TIMEOUT")
        self.assertNotIn("secret", contract.canonical(value).decode())

    def test_setup_and_factory_exceptions_are_redacted(self):
        secrets = [
            "-----BEGIN PRIVATE KEY-----SETUP",
            "Bearer FACTORY-SECRET",
        ]
        values = [
            client.execute(
                contract.build_patch(RV),
                kubeconfig_loader=lambda: (_ for _ in ()).throw(
                    RuntimeError(secrets[0])
                ),
            ),
            client.execute(
                contract.build_patch(RV),
                connection_factory=lambda: (_ for _ in ()).throw(
                    RuntimeError(secrets[1])
                ),
            ),
        ]
        for secret, value in zip(secrets, values):
            self.assertNotIn(secret, contract.canonical(value).decode())
            self.assertEqual(
                contract.classify_exchange(
                    value, contract.build_patch(RV), RV
                )["outcome"],
                "FAIL_CLOSED_TRANSPORT",
            )

    def test_decimal_status_projection_is_json_serializable_and_rejected(self):
        body = (
            b'{"apiVersion":"v1","kind":"Status","metadata":{},'
            b'"status":"Failure","message":"x","reason":"Conflict","code":409,'
            b'"details":{"retryAfterSeconds":1.5}}'
        )
        value = exchange(409, body)
        contract.canonical(value)
        self.assertEqual(
            value["response"]["projectionErrorCode"],
            "NON_JSON_NATIVE_NUMBER_REJECTED",
        )
        self.assert_rejected(value)

    def test_decimal_success_projection_is_json_serializable_and_rejected(self):
        body = success_body().replace(
            b'"resourceVersion":"30000002"',
            b'"resourceVersion":30000002.5',
        )
        value = exchange(200, body)
        contract.canonical(value)
        self.assertEqual(
            value["response"]["projectionErrorCode"],
            "NON_JSON_NATIVE_NUMBER_REJECTED",
        )
        self.assert_rejected(value)

    def test_decimal_success_spec_is_json_serializable_and_rejected(self):
        body = success_body().replace(
            b'"unschedulable":true',
            b'"unexpectedDecimal":1.25,"unschedulable":true',
        )
        value = exchange(200, body)
        contract.canonical(value)
        self.assertEqual(
            value["response"]["projectionErrorCode"],
            "NON_JSON_NATIVE_NUMBER_REJECTED",
        )
        self.assert_rejected(value)

    def test_invalid_http_status_capture_fails_closed(self):
        for status in (99, 600, 999):
            value = exchange(status, status_body({"code": status}))
            self.assertEqual(
                contract.classify_exchange(
                    value, contract.build_patch(RV), RV
                )["outcome"],
                "FAIL_CLOSED_TRANSPORT",
            )

    def test_invalid_http_status_substitution_rejected(self):
        for status in (99, 600, 999):
            value = exchange(409, status_body())
            value["response"]["status"] = status
            value["response"]["statusLine"] = f"HTTP/1.1 {status} OK"
            self.assert_rejected(value)

    def test_response_header_truncation_rejected(self):
        headers = [("Content-Type", "application/json")] + [
            (f"X-{index}", "value") for index in range(70)
        ]
        self.assert_rejected(exchange(409, status_body(), headers))

    def test_substituted_header_count_rejected(self):
        value = exchange(200, success_body())
        value["response"]["headers"] = [("Content-Type", "application/json")] + [
            (f"X-{index}", "value") for index in range(64)
        ]
        value["response"]["headersComplete"] = True
        self.assert_rejected(value)

    def test_http_reason_substitution_rejected(self):
        for reason in ({"secret": "token"}, "x" * 257, "bad\x00reason"):
            value = exchange(200, success_body())
            value["response"]["reason"] = reason
            self.assert_rejected(value)

    def test_http_status_line_substitution_rejected(self):
        value = exchange(200, success_body())
        value["response"]["statusLine"] = "HTTP/1.1 200 substituted"
        self.assert_rejected(value)

    def test_request_authority_and_host_substitution_rejected(self):
        value = exchange(200, success_body())
        value["request"]["authority"] = "attacker.invalid:443"
        value["request"]["headers"][0][1] = "attacker.invalid:443"
        self.assert_rejected(value)

    def test_request_header_order_substitution_rejected(self):
        value = exchange(200, success_body())
        value["request"]["headers"] = list(reversed(value["request"]["headers"]))
        self.assert_rejected(value)

    def test_response_header_value_oversize_rejected(self):
        headers = [
            ("Content-Type", "application/json"),
            ("X-Oversize", "x" * 9000),
        ]
        self.assert_rejected(exchange(409, status_body(), headers))

    def test_response_body_substitution_rejected(self):
        value = exchange(409, status_body())
        value["response"]["bodySHA256"] = "0" * 64
        self.assert_rejected(value)

    def test_projection_substitution_rejected(self):
        value = exchange(409, status_body())
        value["response"]["decodedProjection"]["message"] = "substituted"
        self.assert_rejected(value)

    def test_strict_kubeconfig_duplicate_rejected(self):
        with self.assertRaises(strict_json.StrictJSONError):
            strict_json.loads(b'{"clusters":[],"clusters":[]}', "kubeconfig")

    def test_strict_kubeconfig_non_finite_rejected(self):
        with self.assertRaises(strict_json.StrictJSONError):
            strict_json.loads(b'{"value":NaN}', "kubeconfig")


if __name__ == "__main__":
    unittest.main()
