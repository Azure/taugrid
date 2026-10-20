import base64
import hashlib
import json

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey,
    Ed25519PublicKey,
)


SCHEMA = "dapple-r050708-rev23-first-party-evidence/v1"
ACK_SCHEMA = "dapple-r050708-rev23-sink-ack/v1"
CHECKPOINT_SCHEMA = "dapple-r050708-rev23-witnessed-checkpoint/v1"
TRUST_DOMAIN = "evidence.taugrid.microsoft.com"
ALGORITHM = "Ed25519"
ZERO_HEAD = "0" * 64
EVENTS = (
    "RUN_BOUND",
    "GET_INTENT",
    "GET_SEND_RETURNED",
    "GET_SEND_AMBIGUOUS",
    "GET_RESPONSE",
    "GET_VALIDATED",
    "PATCH_INTENT",
    "PATCH_SEND_RETURNED",
    "PATCH_SEND_AMBIGUOUS",
    "PATCH_RESPONSE",
    "PATCH_VALIDATED_SUCCESS",
    "PATCH_VALIDATED_FAILURE",
    "RUN_TERMINAL",
)


class EvidenceError(ValueError):
    pass


def canonical(value):
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def public_bytes(private_key):
    return private_key.public_key().public_bytes_raw()


def _sign(private_key, value):
    return base64.b64encode(private_key.sign(canonical(value))).decode()


def _verify(public_key_bytes, value, signature):
    try:
        Ed25519PublicKey.from_public_bytes(public_key_bytes).verify(
            base64.b64decode(signature, validate=True), canonical(value)
        )
    except (InvalidSignature, ValueError) as error:
        raise EvidenceError("signature verification failed") from error


def validate_admission(admission, authority_revision):
    required = {
        "schema",
        "trustDomain",
        "algorithm",
        "authorityRevision",
        "serviceKeys",
        "witnessKeys",
        "witnessThreshold",
    }
    if type(admission) is not dict or set(admission) != required:
        raise EvidenceError("admission keys mismatch")
    if (
        admission["schema"] != SCHEMA
        or admission["trustDomain"] != TRUST_DOMAIN
        or admission["algorithm"] != ALGORITHM
        or admission["authorityRevision"] != authority_revision
        or type(admission["serviceKeys"]) is not list
        or type(admission["witnessKeys"]) is not list
        or type(admission["witnessThreshold"]) is not int
        or admission["witnessThreshold"] < 2
        or admission["witnessThreshold"] > len(admission["witnessKeys"])
    ):
        raise EvidenceError("admission policy mismatch")
    identifiers = set()
    for role in ("serviceKeys", "witnessKeys"):
        for key in admission[role]:
            if (
                type(key) is not dict
                or set(key)
                != {
                    "keyID",
                    "publicKeyBase64",
                    "validFromRevision",
                    "validThroughRevision",
                    "revoked",
                }
                or type(key["keyID"]) is not str
                or not key["keyID"]
                or key["keyID"] in identifiers
                or type(key["validFromRevision"]) is not int
                or type(key["validThroughRevision"]) is not int
                or key["validFromRevision"] > key["validThroughRevision"]
                or type(key["revoked"]) is not bool
            ):
                raise EvidenceError("admitted key mismatch")
            try:
                raw = base64.b64decode(
                    key["publicKeyBase64"], validate=True
                )
                Ed25519PublicKey.from_public_bytes(raw)
            except (ValueError, TypeError) as error:
                raise EvidenceError("admitted public key mismatch") from error
            identifiers.add(key["keyID"])


def _admitted_key(admission, role, key_id, authority_revision):
    validate_admission(admission, authority_revision)
    matches = [
        key
        for key in admission[role]
        if key["keyID"] == key_id
        and key["validFromRevision"] <= authority_revision
        and key["validThroughRevision"] >= authority_revision
        and not key["revoked"]
    ]
    if len(matches) != 1:
        raise EvidenceError("signing key is not currently admitted")
    return base64.b64decode(matches[0]["publicKeyBase64"])


def validate_binding(binding):
    if (
        type(binding) is not dict
        or set(binding)
        != {
            "authorityRevision",
            "authoritySHA256",
            "runID",
            "transferID",
            "contractSHA256",
            "generation",
        }
    ):
        raise EvidenceError("run binding mismatch")
    for name in ("authoritySHA256", "contractSHA256"):
        value = binding[name]
        if (
            type(value) is not str
            or len(value) != 64
            or any(character not in "0123456789abcdef" for character in value)
        ):
            raise EvidenceError("run binding digest mismatch")
    if type(binding["authorityRevision"]) is not int:
        raise EvidenceError("authority revision mismatch")
    for name in ("runID", "transferID", "generation"):
        if type(binding[name]) is not str or not binding[name]:
            raise EvidenceError("run binding text mismatch")


def _validate_payload(event, payload, binding, prior_request_hash):
    if type(payload) is not dict:
        raise EvidenceError("event payload mismatch")
    if event == "RUN_BOUND":
        if payload != {
            "authoritySHA256": binding["authoritySHA256"],
            "transferID": binding["transferID"],
            "contractSHA256": binding["contractSHA256"],
        }:
            raise EvidenceError("RUN_BOUND is not cross-bound")
        return None
    if event in {"GET_INTENT", "PATCH_INTENT"}:
        if set(payload) != {"requestSHA256", "requestBytes"}:
            raise EvidenceError("intent payload mismatch")
        if type(payload["requestBytes"]) is not int or payload["requestBytes"] < 1:
            raise EvidenceError("request byte count mismatch")
        request_hash = payload["requestSHA256"]
    else:
        if "requestSHA256" not in payload:
            raise EvidenceError("event request hash missing")
        request_hash = payload["requestSHA256"]
        allowed = {
            "GET_SEND_RETURNED": {"requestSHA256"},
            "GET_SEND_AMBIGUOUS": {"requestSHA256", "errorCode"},
            "GET_RESPONSE": {
                "requestSHA256",
                "status",
                "reason",
                "headersSHA256",
                "bodySHA256",
                "bodyBytes",
            },
            "GET_VALIDATED": {
                "requestSHA256",
                "resourceVersion",
                "predecessorSpecSHA256",
            },
            "PATCH_SEND_RETURNED": {"requestSHA256"},
            "PATCH_SEND_AMBIGUOUS": {"requestSHA256", "errorCode"},
            "PATCH_RESPONSE": {
                "requestSHA256",
                "status",
                "reason",
                "headersSHA256",
                "bodySHA256",
                "bodyBytes",
            },
            "PATCH_VALIDATED_SUCCESS": {
                "requestSHA256",
                "classificationSHA256",
                "resourceVersion",
            },
            "PATCH_VALIDATED_FAILURE": {
                "requestSHA256",
                "classificationSHA256",
                "failureOutcome",
            },
            "RUN_TERMINAL": {"requestSHA256", "outcome"},
        }
        if set(payload) != allowed[event]:
            raise EvidenceError("event payload keys mismatch")
    if (
        type(request_hash) is not str
        or len(request_hash) != 64
        or any(character not in "0123456789abcdef" for character in request_hash)
        or (
            prior_request_hash is not None
            and request_hash != prior_request_hash
            and event not in {"PATCH_INTENT", "RUN_TERMINAL"}
        )
    ):
        raise EvidenceError("request hash cross-binding mismatch")
    if event.endswith("_RESPONSE"):
        if (
            type(payload["status"]) is not int
            or isinstance(payload["status"], bool)
            or not 100 <= payload["status"] <= 599
            or type(payload["reason"]) is not str
            or len(payload["reason"].encode()) > 256
        ):
            raise EvidenceError("HTTP response binding mismatch")
    return request_hash


def make_leaf(binding, sequence, event, payload, previous_head):
    validate_binding(binding)
    if (
        event not in EVENTS
        or type(sequence) is not int
        or sequence < 0
        or type(previous_head) is not str
        or len(previous_head) != 64
    ):
        raise EvidenceError("leaf metadata mismatch")
    return {
        "schema": SCHEMA,
        **binding,
        "sequence": sequence,
        "event": event,
        "payload": payload,
        "previousTreeHead": previous_head,
    }


class FirstPartyEvidenceService:
    def __init__(self, key_id, private_key):
        self.key_id = key_id
        self._private_key = private_key
        self._logs = {}

    def append(self, leaf):
        binding_key = (
            leaf["authoritySHA256"],
            leaf["runID"],
            leaf["transferID"],
            leaf["generation"],
        )
        log = self._logs.setdefault(binding_key, [])
        previous_head = ZERO_HEAD if not log else log[-1]["treeHead"]
        if (
            leaf["sequence"] != len(log)
            or leaf["previousTreeHead"] != previous_head
        ):
            raise EvidenceError("monotonic append precondition failed")
        leaf_hash = digest(canonical(leaf))
        tree_head = digest(bytes.fromhex(previous_head) + bytes.fromhex(leaf_hash))
        entry = {
            "leaf": leaf,
            "leafHash": leaf_hash,
            "treeHead": tree_head,
        }
        log.append(entry)
        unsigned = {
            "schema": ACK_SCHEMA,
            "trustDomain": TRUST_DOMAIN,
            "algorithm": ALGORITHM,
            "keyID": self.key_id,
            "authorityRevision": leaf["authorityRevision"],
            "authoritySHA256": leaf["authoritySHA256"],
            "runID": leaf["runID"],
            "transferID": leaf["transferID"],
            "contractSHA256": leaf["contractSHA256"],
            "generation": leaf["generation"],
            "sequence": leaf["sequence"],
            "event": leaf["event"],
            "requestSHA256": leaf["payload"].get("requestSHA256"),
            "previousTreeHead": previous_head,
            "leafHash": leaf_hash,
            "treeHead": tree_head,
        }
        return {**unsigned, "signatureBase64": _sign(self._private_key, unsigned)}

    def entries(self, binding):
        key = (
            binding["authoritySHA256"],
            binding["runID"],
            binding["transferID"],
            binding["generation"],
        )
        return json.loads(json.dumps(self._logs.get(key, [])))


def verify_ack(ack, leaf, admission):
    required = {
        "schema",
        "trustDomain",
        "algorithm",
        "keyID",
        "authorityRevision",
        "authoritySHA256",
        "runID",
        "transferID",
        "contractSHA256",
        "generation",
        "sequence",
        "event",
        "requestSHA256",
        "previousTreeHead",
        "leafHash",
        "treeHead",
        "signatureBase64",
    }
    if type(ack) is not dict or set(ack) != required:
        raise EvidenceError("ACK keys mismatch")
    unsigned = {key: value for key, value in ack.items() if key != "signatureBase64"}
    expected_leaf_hash = digest(canonical(leaf))
    expected_head = digest(
        bytes.fromhex(leaf["previousTreeHead"])
        + bytes.fromhex(expected_leaf_hash)
    )
    if (
        ack["schema"] != ACK_SCHEMA
        or ack["trustDomain"] != TRUST_DOMAIN
        or ack["algorithm"] != ALGORITHM
        or ack["authorityRevision"] != leaf["authorityRevision"]
        or ack["authoritySHA256"] != leaf["authoritySHA256"]
        or ack["runID"] != leaf["runID"]
        or ack["transferID"] != leaf["transferID"]
        or ack["contractSHA256"] != leaf["contractSHA256"]
        or ack["generation"] != leaf["generation"]
        or ack["sequence"] != leaf["sequence"]
        or ack["event"] != leaf["event"]
        or ack["requestSHA256"] != leaf["payload"].get("requestSHA256")
        or ack["previousTreeHead"] != leaf["previousTreeHead"]
        or ack["leafHash"] != expected_leaf_hash
        or ack["treeHead"] != expected_head
    ):
        raise EvidenceError("ACK binding mismatch")
    public_key = _admitted_key(
        admission,
        "serviceKeys",
        ack["keyID"],
        ack["authorityRevision"],
    )
    _verify(public_key, unsigned, ack["signatureBase64"])


class Witness:
    def __init__(self, key_id, private_key):
        self.key_id = key_id
        self._private_key = private_key
        self._heads = {}

    def sign(self, binding, index, tree_head):
        key = (
            binding["authoritySHA256"],
            binding["runID"],
            binding["transferID"],
            binding["generation"],
            index,
        )
        existing = self._heads.get(key)
        if existing is not None and existing != tree_head:
            raise EvidenceError("witness rejected split view")
        self._heads[key] = tree_head
        unsigned = {
            "trustDomain": TRUST_DOMAIN,
            "algorithm": ALGORITHM,
            "keyID": self.key_id,
            "authorityRevision": binding["authorityRevision"],
            "authoritySHA256": binding["authoritySHA256"],
            "runID": binding["runID"],
            "transferID": binding["transferID"],
            "contractSHA256": binding["contractSHA256"],
            "generation": binding["generation"],
            "index": index,
            "treeHead": tree_head,
        }
        return {**unsigned, "signatureBase64": _sign(self._private_key, unsigned)}


def make_checkpoint(binding, entries, witness_signatures):
    return {
        "schema": CHECKPOINT_SCHEMA,
        **binding,
        "index": len(entries) - 1,
        "treeHead": entries[-1]["treeHead"],
        "witnessSignatures": witness_signatures,
    }


def verify_checkpoint(checkpoint, admission):
    required = {
        "schema",
        "authorityRevision",
        "authoritySHA256",
        "runID",
        "transferID",
        "contractSHA256",
        "generation",
        "index",
        "treeHead",
        "witnessSignatures",
    }
    if type(checkpoint) is not dict or set(checkpoint) != required:
        raise EvidenceError("checkpoint keys mismatch")
    if checkpoint["schema"] != CHECKPOINT_SCHEMA:
        raise EvidenceError("checkpoint schema mismatch")
    valid = set()
    for signature in checkpoint["witnessSignatures"]:
        key_id = signature.get("keyID")
        if key_id in valid:
            raise EvidenceError("duplicate witness")
        unsigned = {
            key: value
            for key, value in signature.items()
            if key != "signatureBase64"
        }
        expected = {
            "trustDomain": TRUST_DOMAIN,
            "algorithm": ALGORITHM,
            "keyID": key_id,
            "authorityRevision": checkpoint["authorityRevision"],
            "authoritySHA256": checkpoint["authoritySHA256"],
            "runID": checkpoint["runID"],
            "transferID": checkpoint["transferID"],
            "contractSHA256": checkpoint["contractSHA256"],
            "generation": checkpoint["generation"],
            "index": checkpoint["index"],
            "treeHead": checkpoint["treeHead"],
        }
        if unsigned != expected:
            raise EvidenceError("witness checkpoint binding mismatch")
        public_key = _admitted_key(
            admission,
            "witnessKeys",
            key_id,
            checkpoint["authorityRevision"],
        )
        _verify(public_key, unsigned, signature["signatureBase64"])
        valid.add(key_id)
    if len(valid) < admission["witnessThreshold"]:
        raise EvidenceError("witness threshold not met")


def validate_log(binding, entries, checkpoint, admission):
    validate_binding(binding)
    verify_checkpoint(checkpoint, admission)
    for name, value in binding.items():
        if checkpoint[name] != value:
            raise EvidenceError("checkpoint run binding mismatch")
    if checkpoint["index"] != len(entries) - 1:
        raise EvidenceError("stale prefix or missing suffix")
    prior_head = ZERO_HEAD
    prior_request_hash = None
    events = []
    transitions = {
        "RUN_BOUND": {"GET_INTENT", "RUN_TERMINAL"},
        "GET_INTENT": {
            "GET_SEND_RETURNED",
            "GET_SEND_AMBIGUOUS",
            "GET_RESPONSE",
            "RUN_TERMINAL",
        },
        "GET_SEND_RETURNED": {"GET_RESPONSE", "RUN_TERMINAL"},
        "GET_SEND_AMBIGUOUS": {"GET_RESPONSE", "RUN_TERMINAL"},
        "GET_RESPONSE": {"GET_VALIDATED", "RUN_TERMINAL"},
        "GET_VALIDATED": {"PATCH_INTENT", "RUN_TERMINAL"},
        "PATCH_INTENT": {
            "PATCH_SEND_RETURNED",
            "PATCH_SEND_AMBIGUOUS",
            "PATCH_RESPONSE",
            "RUN_TERMINAL",
        },
        "PATCH_SEND_RETURNED": {"PATCH_RESPONSE", "RUN_TERMINAL"},
        "PATCH_SEND_AMBIGUOUS": {"PATCH_RESPONSE", "RUN_TERMINAL"},
        "PATCH_RESPONSE": {
            "PATCH_VALIDATED_SUCCESS",
            "PATCH_VALIDATED_FAILURE",
            "RUN_TERMINAL",
        },
        "PATCH_VALIDATED_SUCCESS": {"RUN_TERMINAL"},
        "PATCH_VALIDATED_FAILURE": {"RUN_TERMINAL"},
        "RUN_TERMINAL": set(),
    }
    for index, entry in enumerate(entries):
        leaf = entry["leaf"]
        if (
            leaf["sequence"] != index
            or leaf["previousTreeHead"] != prior_head
            or any(leaf[name] != value for name, value in binding.items())
            or entry["leafHash"] != digest(canonical(leaf))
        ):
            raise EvidenceError("log leaf binding mismatch")
        expected_head = digest(
            bytes.fromhex(prior_head) + bytes.fromhex(entry["leafHash"])
        )
        if entry["treeHead"] != expected_head:
            raise EvidenceError("log tree head mismatch")
        event = leaf["event"]
        if events and event not in transitions[events[-1]]:
            raise EvidenceError("event transition mismatch")
        request_hash = _validate_payload(
            event, leaf["payload"], binding, prior_request_hash
        )
        if event in {"GET_INTENT", "PATCH_INTENT"}:
            prior_request_hash = request_hash
        events.append(event)
        prior_head = expected_head
    if checkpoint["treeHead"] != prior_head:
        raise EvidenceError("checkpoint tree head mismatch")
    return recover_state(entries)


def authorize_send(intent_leaf, ack, admission):
    if intent_leaf["event"] not in {"GET_INTENT", "PATCH_INTENT"}:
        raise EvidenceError("only an acknowledged intent can authorize send")
    verify_ack(ack, intent_leaf, admission)
    return {
        "runID": intent_leaf["runID"],
        "sequence": intent_leaf["sequence"],
        "requestSHA256": intent_leaf["payload"]["requestSHA256"],
        "ackTreeHead": ack["treeHead"],
    }


def recover_state(entries):
    events = [entry["leaf"]["event"] for entry in entries]
    event_set = set(events)
    get_attempted = "GET_INTENT" in event_set
    patch_attempted = "PATCH_INTENT" in event_set
    get_completed = "GET_RESPONSE" in event_set
    patch_completed = "PATCH_RESPONSE" in event_set
    minimum = int(get_completed) + int(patch_completed)
    maximum = int(get_attempted) + int(patch_attempted)
    if "PATCH_VALIDATED_SUCCESS" in event_set:
        applied = "YES"
        states = ["rows05-08-remediation-20261019t080808z-v3"]
    elif "PATCH_VALIDATED_FAILURE" in event_set:
        applied = "NO"
        states = ["rows05-08-remediation-20261019t003520z-v2"]
    elif patch_attempted:
        applied = "UNKNOWN"
        states = [
            "rows05-08-remediation-20261019t003520z-v2",
            "rows05-08-remediation-20261019t080808z-v3",
        ]
    else:
        applied = "NO"
        states = ["rows05-08-remediation-20261019t003520z-v2"]
    return {
        "schema": "dapple-r050708-rev23-recovered-state/v1",
        "directAPICallsObserved": minimum if minimum == maximum else None,
        "directAPICallsPossibleMinimum": minimum,
        "directAPICallsPossibleMaximum": maximum,
        "getAttempted": get_attempted,
        "getCompleted": get_completed,
        "patchAttempted": patch_attempted,
        "patchCompleted": patch_completed,
        "patchApplied": applied,
        "reservationPossibleStates": states,
        "terminalRecordPresent": "RUN_TERMINAL" in event_set,
    }
