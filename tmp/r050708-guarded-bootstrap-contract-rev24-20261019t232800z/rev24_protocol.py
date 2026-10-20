import base64
import copy
import hashlib

from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey,
)

import evidence_protocol as core


ADMISSION_SCHEMA = "dapple-r050708-rev24-authority-admission/v1"
CHALLENGE_SCHEMA = "dapple-r050708-rev24-run-challenge/v1"
CONSUMPTION_SCHEMA = "dapple-r050708-rev24-ack-consumption/v1"
DEPLOYMENT_SCHEMA = "dapple-r050708-rev24-deployment-acceptance/v1"
FRESHNESS_SCHEMA = "dapple-r050708-rev24-latest-checkpoint/v1"
EXACT_WITNESS_COUNT = 3
EXACT_THRESHOLD = 2
MAX_BODY_BYTES = 16 * 1024 * 1024


class Rev24Error(core.EvidenceError):
    pass


def _sign(private_key, value):
    return base64.b64encode(
        private_key.sign(core.canonical(value))
    ).decode()


def _verify(public_key, value, signature):
    core._verify(public_key, value, signature)


def _hex(value, name, length=64):
    if (
        type(value) is not str
        or len(value) != length
        or any(character not in "0123456789abcdef" for character in value)
    ):
        raise Rev24Error(f"{name} format mismatch")


def sign_authority_statement(statement, root_private_key):
    return {
        "statement": copy.deepcopy(statement),
        "signatureBase64": _sign(root_private_key, statement),
    }


def verify_authority_statement(
    envelope, root_public_key, schema, ledger_revision, ledger_sha256
):
    if (
        type(envelope) is not dict
        or set(envelope) != {"statement", "signatureBase64"}
        or type(envelope["statement"]) is not dict
    ):
        raise Rev24Error("authority envelope mismatch")
    statement = envelope["statement"]
    if (
        statement.get("schema") != schema
        or statement.get("ledgerRevision") != ledger_revision
        or statement.get("ledgerSHA256") != ledger_sha256
    ):
        raise Rev24Error("canonical authority anchor mismatch")
    _hex(statement["ledgerSHA256"], "ledger SHA256")
    _verify(root_public_key, statement, envelope["signatureBase64"])
    return statement


def verify_admission(
    envelope, root_public_key, ledger_revision, ledger_sha256
):
    statement = verify_authority_statement(
        envelope,
        root_public_key,
        ADMISSION_SCHEMA,
        ledger_revision,
        ledger_sha256,
    )
    required = {
        "schema",
        "ledgerRevision",
        "ledgerSHA256",
        "trustDomain",
        "algorithm",
        "serviceKeys",
        "witnessKeys",
        "witnessThreshold",
        "deploymentAcceptanceID",
    }
    if (
        set(statement) != required
        or statement["trustDomain"] != core.TRUST_DOMAIN
        or statement["algorithm"] != core.ALGORITHM
        or type(statement["serviceKeys"]) is not list
        or len(statement["serviceKeys"]) != 1
        or type(statement["witnessKeys"]) is not list
        or len(statement["witnessKeys"]) != EXACT_WITNESS_COUNT
        or statement["witnessThreshold"] != EXACT_THRESHOLD
    ):
        raise Rev24Error("closed admission policy mismatch")
    if (
        type(statement["deploymentAcceptanceID"]) is not str
        or not statement["deploymentAcceptanceID"]
    ):
        raise Rev24Error("deployment acceptance ID mismatch")
    ids = set()
    public_keys = set()
    roles = (
        [("service", key) for key in statement["serviceKeys"]]
        + [("witness", key) for key in statement["witnessKeys"]]
    )
    for expected_role, key in roles:
        if (
            type(key) is not dict
            or set(key)
            != {
                "role",
                "keyID",
                "publicKeyBase64",
                "validFromRevision",
                "validThroughRevision",
                "revoked",
            }
            or key["role"] != expected_role
            or key["keyID"] in ids
            or key["publicKeyBase64"] in public_keys
        ):
            raise Rev24Error("cross-role key uniqueness mismatch")
        ids.add(key["keyID"])
        public_keys.add(key["publicKeyBase64"])
    core.validate_admission(
        {
            "schema": core.SCHEMA,
            "trustDomain": statement["trustDomain"],
            "algorithm": statement["algorithm"],
            "authorityRevision": ledger_revision,
            "serviceKeys": [
                {name: value for name, value in key.items() if name != "role"}
                for key in statement["serviceKeys"]
            ],
            "witnessKeys": [
                {name: value for name, value in key.items() if name != "role"}
                for key in statement["witnessKeys"]
            ],
            "witnessThreshold": statement["witnessThreshold"],
        },
        ledger_revision,
    )
    return statement


def verify_deployment(
    envelope,
    root_public_key,
    ledger_revision,
    ledger_sha256,
    admission,
):
    statement = verify_authority_statement(
        envelope,
        root_public_key,
        DEPLOYMENT_SCHEMA,
        ledger_revision,
        ledger_sha256,
    )
    required = {
        "schema",
        "ledgerRevision",
        "ledgerSHA256",
        "acceptanceID",
        "serviceIdentity",
        "serviceEndpoint",
        "witnessIdentities",
        "witnessEndpoints",
        "keyCustodyEvidenceSHA256",
        "quorumReachable",
        "durablePersistence",
        "retentionLocked",
        "backupVerified",
        "freshnessGossipHealthy",
        "admissionSHA256",
    }
    if (
        set(statement) != required
        or len(statement["witnessIdentities"]) != EXACT_WITNESS_COUNT
        or len(set(statement["witnessIdentities"])) != EXACT_WITNESS_COUNT
        or len(statement["witnessEndpoints"]) != EXACT_WITNESS_COUNT
        or len(set(statement["witnessEndpoints"])) != EXACT_WITNESS_COUNT
        or not all(
            statement[name] is True
            for name in (
                "quorumReachable",
                "durablePersistence",
                "retentionLocked",
                "backupVerified",
                "freshnessGossipHealthy",
            )
        )
    ):
        raise Rev24Error("deployment prerequisite mismatch")
    for name in (
        "keyCustodyEvidenceSHA256",
        "admissionSHA256",
    ):
        _hex(statement[name], name)
    expected_admission_hash = core.digest(core.canonical(admission))
    if statement["admissionSHA256"] != expected_admission_hash:
        raise Rev24Error("deployment admission binding mismatch")
    return statement


def verify_challenge(
    envelope,
    root_public_key,
    ledger_revision,
    ledger_sha256,
    binding,
    deployment_hash,
    now_epoch,
):
    statement = verify_authority_statement(
        envelope,
        root_public_key,
        CHALLENGE_SCHEMA,
        ledger_revision,
        ledger_sha256,
    )
    required = {
        "schema",
        "ledgerRevision",
        "ledgerSHA256",
        "authorityRevision",
        "authoritySHA256",
        "runID",
        "transferID",
        "contractSHA256",
        "generation",
        "challengeNonce",
        "issuedAtEpoch",
        "expiresAtEpoch",
        "priorCheckpointIndex",
        "priorCheckpointHead",
        "deploymentAcceptanceSHA256",
    }
    if (
        set(statement) != required
        or any(statement[name] != value for name, value in binding.items())
        or type(statement["issuedAtEpoch"]) is not int
        or type(statement["expiresAtEpoch"]) is not int
        or statement["issuedAtEpoch"] > now_epoch
        or statement["expiresAtEpoch"] < now_epoch
        or statement["expiresAtEpoch"] - statement["issuedAtEpoch"] > 900
        or type(statement["priorCheckpointIndex"]) is not int
        or statement["priorCheckpointIndex"] < -1
        or statement["deploymentAcceptanceSHA256"] != deployment_hash
    ):
        raise Rev24Error("run challenge mismatch")
    _hex(statement["challengeNonce"], "challenge nonce")
    _hex(statement["priorCheckpointHead"], "prior checkpoint head")
    return statement


def admitted_core(admission):
    return {
        "schema": core.SCHEMA,
        "trustDomain": admission["trustDomain"],
        "algorithm": admission["algorithm"],
        "authorityRevision": admission["ledgerRevision"],
        "serviceKeys": [
            {name: value for name, value in key.items() if name != "role"}
            for key in admission["serviceKeys"]
        ],
        "witnessKeys": [
            {name: value for name, value in key.items() if name != "role"}
            for key in admission["witnessKeys"]
        ],
        "witnessThreshold": admission["witnessThreshold"],
    }


class FreshEvidenceService:
    def __init__(self, key_id, private_key):
        self.core = core.FirstPartyEvidenceService(key_id, private_key)
        self.key_id = key_id
        self._private_key = private_key
        self._consumed = set()
        self._proofs = {}

    def append(self, leaf):
        return self.core.append(leaf)

    def consume_intent_ack(
        self, leaf, ack, admission, challenge, now_epoch
    ):
        if leaf["event"] not in {"GET_INTENT", "PATCH_INTENT"}:
            raise Rev24Error("only intent ACK can be consumed")
        if now_epoch > challenge["expiresAtEpoch"]:
            raise Rev24Error("ACK expired")
        if (
            leaf["authoritySHA256"] != challenge["authoritySHA256"]
            or leaf["runID"] != challenge["runID"]
            or leaf["transferID"] != challenge["transferID"]
            or leaf["contractSHA256"] != challenge["contractSHA256"]
            or leaf["generation"] != challenge["generation"]
        ):
            raise Rev24Error("ACK challenge cross-run mismatch")
        core.verify_ack(ack, leaf, admitted_core(admission))
        consumption_id = (
            challenge["challengeNonce"],
            leaf["runID"],
            leaf["sequence"],
            leaf["event"],
            leaf["payload"]["requestSHA256"],
        )
        if consumption_id in self._consumed:
            raise Rev24Error("ACK replay or replay race")
        self._consumed.add(consumption_id)
        unsigned = {
            "schema": CONSUMPTION_SCHEMA,
            "trustDomain": core.TRUST_DOMAIN,
            "algorithm": core.ALGORITHM,
            "keyID": self.key_id,
            "authorityRevision": leaf["authorityRevision"],
            "authoritySHA256": leaf["authoritySHA256"],
            "runID": leaf["runID"],
            "transferID": leaf["transferID"],
            "contractSHA256": leaf["contractSHA256"],
            "generation": leaf["generation"],
            "challengeNonce": challenge["challengeNonce"],
            "expiresAtEpoch": challenge["expiresAtEpoch"],
            "sequence": leaf["sequence"],
            "event": leaf["event"],
            "requestSHA256": leaf["payload"]["requestSHA256"],
            "ackTreeHead": ack["treeHead"],
            "consumedAtEpoch": now_epoch,
        }
        proof = {
            **unsigned,
            "signatureBase64": _sign(self._private_key, unsigned),
        }
        self._proofs[leaf["sequence"]] = {
            "ack": copy.deepcopy(ack),
            "consumption": proof,
        }
        return proof

    def entries_with_proofs(self, binding):
        entries = self.core.entries(binding)
        for index, entry in enumerate(entries):
            entry["intentProof"] = copy.deepcopy(self._proofs.get(index))
        return entries


def verify_intent_proof(
    entry, admission, challenge, expected_sequence
):
    leaf = entry["leaf"]
    proof = entry.get("intentProof")
    if leaf["event"] not in {"GET_INTENT", "PATCH_INTENT"}:
        if proof is not None:
            raise Rev24Error("non-intent entry contains ACK proof")
        return
    if type(proof) is not dict or set(proof) != {"ack", "consumption"}:
        raise Rev24Error("recovery intent ACK proof missing")
    core.verify_ack(proof["ack"], leaf, admitted_core(admission))
    consumption = proof["consumption"]
    if type(consumption) is not dict or "signatureBase64" not in consumption:
        raise Rev24Error("consumption proof missing")
    unsigned = {
        key: value
        for key, value in consumption.items()
        if key != "signatureBase64"
    }
    expected = {
        "schema": CONSUMPTION_SCHEMA,
        "trustDomain": core.TRUST_DOMAIN,
        "algorithm": core.ALGORITHM,
        "keyID": proof["ack"]["keyID"],
        "authorityRevision": leaf["authorityRevision"],
        "authoritySHA256": leaf["authoritySHA256"],
        "runID": leaf["runID"],
        "transferID": leaf["transferID"],
        "contractSHA256": leaf["contractSHA256"],
        "generation": leaf["generation"],
        "challengeNonce": challenge["challengeNonce"],
        "expiresAtEpoch": challenge["expiresAtEpoch"],
        "sequence": expected_sequence,
        "event": leaf["event"],
        "requestSHA256": leaf["payload"]["requestSHA256"],
        "ackTreeHead": proof["ack"]["treeHead"],
        "consumedAtEpoch": consumption.get("consumedAtEpoch"),
    }
    if (
        unsigned != expected
        or type(consumption.get("consumedAtEpoch")) is not int
        or consumption["consumedAtEpoch"] > challenge["expiresAtEpoch"]
    ):
        raise Rev24Error("consumption proof binding mismatch")
    public_key = core._admitted_key(
        admitted_core(admission),
        "serviceKeys",
        consumption["keyID"],
        leaf["authorityRevision"],
    )
    _verify(public_key, unsigned, consumption["signatureBase64"])


def validate_entries(binding, entries, admission, challenge):
    prior_head = core.ZERO_HEAD
    prior_request = None
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
    if not entries or entries[0]["leaf"]["event"] != "RUN_BOUND":
        raise Rev24Error("RUN_BOUND is mandatory")
    for index, entry in enumerate(entries):
        if set(entry) != {"leaf", "leafHash", "treeHead", "intentProof"}:
            raise Rev24Error("entry closure mismatch")
        leaf = entry["leaf"]
        if (
            leaf["sequence"] != index
            or leaf["previousTreeHead"] != prior_head
            or any(leaf[name] != value for name, value in binding.items())
            or entry["leafHash"] != core.digest(core.canonical(leaf))
        ):
            raise Rev24Error("entry chain mismatch")
        expected_head = core.digest(
            bytes.fromhex(prior_head) + bytes.fromhex(entry["leafHash"])
        )
        if entry["treeHead"] != expected_head:
            raise Rev24Error("tree head mismatch")
        event = leaf["event"]
        if events and event not in transitions[events[-1]]:
            raise Rev24Error("state transition mismatch")
        request = core._validate_payload(
            event, leaf["payload"], binding, prior_request
        )
        if event in {"GET_INTENT", "PATCH_INTENT"}:
            prior_request = request
        if event.endswith("_RESPONSE"):
            payload = leaf["payload"]
            if (
                type(payload["bodyBytes"]) is not int
                or isinstance(payload["bodyBytes"], bool)
                or payload["bodyBytes"] < 0
                or payload["bodyBytes"] > MAX_BODY_BYTES
            ):
                raise Rev24Error("response body byte count mismatch")
            for name in ("headersSHA256", "bodySHA256"):
                _hex(payload[name], name)
        verify_intent_proof(entry, admission, challenge, index)
        events.append(event)
        prior_head = expected_head
    prior_index = challenge["priorCheckpointIndex"]
    prior_checkpoint_head = challenge["priorCheckpointHead"]
    if prior_index == -1:
        if prior_checkpoint_head != core.ZERO_HEAD:
            raise Rev24Error("genesis consistency mismatch")
    elif (
        prior_index >= len(entries)
        or entries[prior_index]["treeHead"] != prior_checkpoint_head
    ):
        raise Rev24Error("prior checkpoint consistency mismatch")
    return core.recover_state(entries)


class ValidatingWitness:
    def __init__(self, key_id, private_key):
        self.key_id = key_id
        self._witness = core.Witness(key_id, private_key)

    def sign_log(
        self, binding, entries, admission, challenge, checkpoint_head
    ):
        validate_entries(binding, entries, admission, challenge)
        if entries[-1]["treeHead"] != checkpoint_head:
            raise Rev24Error("witness refused arbitrary head")
        return self._witness.sign(
            binding, len(entries) - 1, checkpoint_head
        )


def verify_recovery(
    binding,
    entries,
    checkpoint,
    freshness_envelope,
    admission_envelope,
    deployment_envelope,
    challenge_envelope,
    root_public_key,
    ledger_revision,
    ledger_sha256,
    now_epoch,
):
    admission = verify_admission(
        admission_envelope,
        root_public_key,
        ledger_revision,
        ledger_sha256,
    )
    admission_hash = core.digest(core.canonical(admission))
    deployment = verify_deployment(
        deployment_envelope,
        root_public_key,
        ledger_revision,
        ledger_sha256,
        admission,
    )
    deployment_hash = core.digest(core.canonical(deployment))
    if admission["deploymentAcceptanceID"] != deployment["acceptanceID"]:
        raise Rev24Error("admission deployment binding mismatch")
    challenge = verify_challenge(
        challenge_envelope,
        root_public_key,
        ledger_revision,
        ledger_sha256,
        binding,
        deployment_hash,
        now_epoch,
    )
    state = validate_entries(binding, entries, admission, challenge)
    core.verify_checkpoint(checkpoint, admitted_core(admission))
    freshness = verify_authority_statement(
        freshness_envelope,
        root_public_key,
        FRESHNESS_SCHEMA,
        ledger_revision,
        ledger_sha256,
    )
    required = {
        "schema",
        "ledgerRevision",
        "ledgerSHA256",
        "authorityRevision",
        "authoritySHA256",
        "runID",
        "transferID",
        "contractSHA256",
        "generation",
        "challengeNonce",
        "latestIndex",
        "latestTreeHead",
        "observedAtEpoch",
        "admissionSHA256",
        "deploymentAcceptanceSHA256",
    }
    if (
        set(freshness) != required
        or any(freshness[name] != value for name, value in binding.items())
        or freshness["challengeNonce"] != challenge["challengeNonce"]
        or freshness["latestIndex"] != len(entries) - 1
        or freshness["latestTreeHead"] != entries[-1]["treeHead"]
        or checkpoint["index"] != freshness["latestIndex"]
        or checkpoint["treeHead"] != freshness["latestTreeHead"]
        or freshness["admissionSHA256"] != admission_hash
        or freshness["deploymentAcceptanceSHA256"] != deployment_hash
        or type(freshness["observedAtEpoch"]) is not int
        or freshness["observedAtEpoch"] > now_epoch
    ):
        raise Rev24Error("stale checkpoint or withheld suffix")
    return state
