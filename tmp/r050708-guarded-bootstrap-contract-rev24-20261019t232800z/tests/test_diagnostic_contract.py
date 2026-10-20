import base64
import copy
import json
import unittest

import diagnostic_contract as contract
import raw_patch_client as client


NEW_TRANSFER = "00000000-0000-4000-8000-000000000017"
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
    def __init__(self, response=None, error=None):
        self.response = response
        self.error = error
        self.method = None
        self.path = None
        self.headers = []
        self.body = None

    def putrequest(self, method, path, **kwargs):
        self.method = method
        self.path = path

    def putheader(self, name, value):
        self.headers.append([name, value])

    def endheaders(self, body):
        self.body = body
        if self.error:
            raise self.error

    def getresponse(self):
        return self.response

    def close(self):
        pass


def status_body(code, reason, message):
    return contract.canonical(
        {
            "apiVersion": "v1",
            "kind": "Status",
            "metadata": {},
            "status": "Failure",
            "message": message,
            "reason": reason,
            "code": code,
        }
    )


def exchange(status, reason, body, content_type="application/json"):
    connection = FakeConnection(
        FakeResponse(status, [("Content-Type", content_type)], body, reason)
    )
    return client.execute(
        contract.build_patch(RV), connection_factory=lambda: connection
    )


def execution():
    return {
        "transferID": NEW_TRANSFER,
        "transferStateBefore": "ACCEPTED_UNCONSUMED",
        "transferConsumed": True,
        "calls": ["compact-near-write-get", "raw-diagnostic-patch"],
        "retryCount": 0,
        "fallbackCount": 0,
        "postStateGetCount": 0,
        "extraCallCount": 0,
    }


class DiagnosticContractTests(unittest.TestCase):
    def test_success(self):
        body = contract.canonical(
            {
                "apiVersion": "v1",
                "kind": "Node",
                "metadata": {
                    "name": "gpu-07-03",
                    "uid": contract.NODE_UID,
                    "resourceVersion": "30000002",
                },
                "spec": contract.TARGET_SPEC,
                "status": {},
            }
        )
        result = contract.classify_exchange(
            exchange(200, "OK", body), contract.build_patch(RV), RV
        )
        self.assertEqual(result["outcome"], "SUCCESS")

    def test_json_patch_conflict(self):
        result = contract.classify_exchange(
            exchange(409, "Conflict", status_body(409, "Conflict", "test failed")),
            contract.build_patch(RV),
            RV,
        )
        self.assertEqual(result["outcome"], "FAIL_CLOSED_JSON_PATCH_CONFLICT")

    def test_admission_denial(self):
        result = contract.classify_exchange(
            exchange(
                403,
                "Forbidden",
                status_body(403, "Forbidden", "admission denied"),
            ),
            contract.build_patch(RV),
            RV,
        )
        self.assertEqual(
            result["outcome"], "FAIL_CLOSED_ADMISSION_OR_AUTHORIZATION_DENIAL"
        )

    def test_schema_validation_denial(self):
        result = contract.classify_exchange(
            exchange(422, "Invalid", status_body(422, "Invalid", "invalid spec")),
            contract.build_patch(RV),
            RV,
        )
        self.assertEqual(
            result["outcome"], "FAIL_CLOSED_SCHEMA_OR_VALIDATION_DENIAL"
        )

    def test_non_json_error_rejected(self):
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(
                exchange(500, "Error", b"not-json"),
                contract.build_patch(RV),
                RV,
            )

    def test_transport_failure(self):
        connection = FakeConnection(error=TimeoutError("timed out"))
        value = client.execute(
            contract.build_patch(RV), connection_factory=lambda: connection
        )
        result = contract.classify_exchange(value, contract.build_patch(RV), RV)
        self.assertEqual(result["outcome"], "FAIL_CLOSED_TRANSPORT")

    def test_malformed_status_rejected(self):
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(
                exchange(400, "Bad Request", contract.canonical({"kind": "Status"})),
                contract.build_patch(RV),
                RV,
            )

    def test_wrong_content_type_rejected(self):
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(
                exchange(
                    409,
                    "Conflict",
                    status_body(409, "Conflict", "test failed"),
                    "text/plain",
                ),
                contract.build_patch(RV),
                RV,
            )

    def test_missing_body_rejected(self):
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(
                exchange(400, "Bad Request", b""),
                contract.build_patch(RV),
                RV,
            )

    def test_truncated_body_rejected(self):
        value = exchange(
            409, "Conflict", status_body(409, "Conflict", "test failed")
        )
        value["response"]["bodyComplete"] = False
        value["response"]["bodyTruncated"] = True
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(value, contract.build_patch(RV), RV)

    def test_extra_calls_rejected(self):
        value = execution()
        value["extraCallCount"] = 1
        with self.assertRaises(contract.ContractError):
            contract.validate_execution(value, NEW_TRANSFER)

    def test_retry_rejected(self):
        value = execution()
        value["retryCount"] = 1
        with self.assertRaises(contract.ContractError):
            contract.validate_execution(value, NEW_TRANSFER)

    def test_post_state_get_rejected(self):
        value = execution()
        value["postStateGetCount"] = 1
        with self.assertRaises(contract.ContractError):
            contract.validate_execution(value, NEW_TRANSFER)

    def test_stale_rv_rejected(self):
        value = contract.build_patch(RV)
        value[1]["value"] = "stale"
        with self.assertRaises(contract.ContractError):
            contract.validate_patch(value, RV)

    def test_stale_spec_rejected(self):
        value = contract.build_patch(RV)
        value[2]["value"]["unschedulable"] = False
        with self.assertRaises(contract.ContractError):
            contract.validate_patch(value, RV)

    def test_transfer_reuse_rejected(self):
        value = execution()
        value["transferID"] = contract.OLD_TRANSFER_ID
        with self.assertRaises(contract.ContractError):
            contract.validate_execution(value, contract.OLD_TRANSFER_ID)

    def test_evidence_substitution_rejected(self):
        value = exchange(
            409, "Conflict", status_body(409, "Conflict", "test failed")
        )
        body = base64.b64decode(value["request"]["bodyBase64"])
        value["request"]["bodySHA256"] = "0" * 64
        self.assertNotEqual(value["request"]["bodySHA256"], contract.digest(body))
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(value, contract.build_patch(RV), RV)

    def test_wrong_wire_content_type_rejected(self):
        value = exchange(
            409, "Conflict", status_body(409, "Conflict", "test failed")
        )
        for header in value["request"]["headers"]:
            if header[0] == "Content-Type":
                header[1] = "application/json"
        with self.assertRaises(contract.ContractError):
            contract.classify_exchange(value, contract.build_patch(RV), RV)

    def test_execution_happy_path(self):
        contract.validate_execution(execution(), NEW_TRANSFER)


if __name__ == "__main__":
    unittest.main()
