import base64
import copy
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import (
    Ed25519PrivateKey,
)

import evidence_protocol as core
import rev24_protocol as rev24


REVISION = 355
LEDGER_SHA = "a" * 64
NOW = 1000
BINDING = {
    "authorityRevision": REVISION,
    "authoritySHA256": "b" * 64,
    "runID": "rev24-run",
    "transferID": "future-transfer",
    "contractSHA256": "c" * 64,
    "generation": "generation-1",
}
GET_HASH = "1" * 64
PATCH_HASH = "2" * 64


def key_entry(role, key_id, private_key):
    return {
        "role": role,
        "keyID": key_id,
        "publicKeyBase64": base64.b64encode(
            core.public_bytes(private_key)
        ).decode(),
        "validFromRevision": REVISION,
        "validThroughRevision": REVISION,
        "revoked": False,
    }


def payload(event):
    values = {
        "RUN_BOUND": {
            "authoritySHA256": BINDING["authoritySHA256"],
            "transferID": BINDING["transferID"],
            "contractSHA256": BINDING["contractSHA256"],
        },
        "GET_INTENT": {"requestSHA256": GET_HASH, "requestBytes": 100},
        "GET_SEND_RETURNED": {"requestSHA256": GET_HASH},
        "GET_SEND_AMBIGUOUS": {
            "requestSHA256": GET_HASH,
            "errorCode": "SEND_UNKNOWN",
        },
        "GET_RESPONSE": {
            "requestSHA256": GET_HASH,
            "status": 200,
            "reason": "OK",
            "headersSHA256": "3" * 64,
            "bodySHA256": "4" * 64,
            "bodyBytes": 1000,
        },
        "GET_VALIDATED": {
            "requestSHA256": GET_HASH,
            "resourceVersion": "30000005",
            "predecessorSpecSHA256": "5" * 64,
        },
        "PATCH_INTENT": {
            "requestSHA256": PATCH_HASH,
            "requestBytes": 400,
        },
        "PATCH_SEND_RETURNED": {"requestSHA256": PATCH_HASH},
        "PATCH_SEND_AMBIGUOUS": {
            "requestSHA256": PATCH_HASH,
            "errorCode": "SEND_UNKNOWN",
        },
        "PATCH_RESPONSE": {
            "requestSHA256": PATCH_HASH,
            "status": 200,
            "reason": "OK",
            "headersSHA256": "6" * 64,
            "bodySHA256": "7" * 64,
            "bodyBytes": 2000,
        },
        "PATCH_VALIDATED_SUCCESS": {
            "requestSHA256": PATCH_HASH,
            "classificationSHA256": "8" * 64,
            "resourceVersion": "30000006",
        },
        "PATCH_VALIDATED_FAILURE": {
            "requestSHA256": PATCH_HASH,
            "classificationSHA256": "9" * 64,
            "failureOutcome": "ATOMIC_PATCH_REJECTED",
        },
        "RUN_TERMINAL": {
            "requestSHA256": PATCH_HASH,
            "outcome": "PASS",
        },
    }
    return copy.deepcopy(values[event])


class Fixture:
    def __init__(self):
        self.root = Ed25519PrivateKey.generate()
        self.service_key = Ed25519PrivateKey.generate()
        self.witness_keys = [
            Ed25519PrivateKey.generate() for _ in range(3)
        ]
        admission = {
            "schema": rev24.ADMISSION_SCHEMA,
            "ledgerRevision": REVISION,
            "ledgerSHA256": LEDGER_SHA,
            "trustDomain": core.TRUST_DOMAIN,
            "algorithm": core.ALGORITHM,
            "serviceKeys": [
                key_entry("service", "service-1", self.service_key)
            ],
            "witnessKeys": [
                key_entry("witness", f"witness-{index}", key)
                for index, key in enumerate(self.witness_keys)
            ],
            "witnessThreshold": 2,
            "deploymentAcceptanceID": "deployment-accepted-1",
        }
        self.admission_envelope = rev24.sign_authority_statement(
            admission, self.root
        )
        admission_hash = core.digest(core.canonical(admission))
        deployment = {
            "schema": rev24.DEPLOYMENT_SCHEMA,
            "ledgerRevision": REVISION,
            "ledgerSHA256": LEDGER_SHA,
            "acceptanceID": "deployment-accepted-1",
            "serviceIdentity": "taugrid-evidence-service",
            "serviceEndpoint": "https://evidence.taugrid.microsoft.com",
            "witnessIdentities": ["w0", "w1", "w2"],
            "witnessEndpoints": [
                "https://w0.evidence.taugrid.microsoft.com",
                "https://w1.evidence.taugrid.microsoft.com",
                "https://w2.evidence.taugrid.microsoft.com",
            ],
            "keyCustodyEvidenceSHA256": "d" * 64,
            "quorumReachable": True,
            "durablePersistence": True,
            "retentionLocked": True,
            "backupVerified": True,
            "freshnessGossipHealthy": True,
            "admissionSHA256": admission_hash,
        }
        self.deployment_envelope = rev24.sign_authority_statement(
            deployment, self.root
        )
        self.deployment_hash = core.digest(core.canonical(deployment))
        challenge = {
            "schema": rev24.CHALLENGE_SCHEMA,
            "ledgerRevision": REVISION,
            "ledgerSHA256": LEDGER_SHA,
            **BINDING,
            "challengeNonce": "e" * 64,
            "issuedAtEpoch": NOW - 10,
            "expiresAtEpoch": NOW + 100,
            "priorCheckpointIndex": -1,
            "priorCheckpointHead": core.ZERO_HEAD,
            "deploymentAcceptanceSHA256": self.deployment_hash,
        }
        self.challenge_envelope = rev24.sign_authority_statement(
            challenge, self.root
        )
        self.admission = rev24.verify_admission(
            self.admission_envelope,
            core.public_bytes(self.root),
            REVISION,
            LEDGER_SHA,
        )
        self.challenge = rev24.verify_challenge(
            self.challenge_envelope,
            core.public_bytes(self.root),
            REVISION,
            LEDGER_SHA,
            BINDING,
            self.deployment_hash,
            NOW,
        )
        self.service = rev24.FreshEvidenceService(
            "service-1", self.service_key
        )
        self.witnesses = [
            rev24.ValidatingWitness(f"witness-{index}", key)
            for index, key in enumerate(self.witness_keys)
        ]

    def execute(self, events):
        head = core.ZERO_HEAD
        for sequence, event in enumerate(events):
            leaf = core.make_leaf(
                BINDING, sequence, event, payload(event), head
            )
            ack = self.service.append(leaf)
            if event in {"GET_INTENT", "PATCH_INTENT"}:
                self.service.consume_intent_ack(
                    leaf, ack, self.admission, self.challenge, NOW
                )
            head = ack["treeHead"]
        entries = self.service.entries_with_proofs(BINDING)
        signatures = [
            witness.sign_log(
                BINDING,
                entries,
                self.admission,
                self.challenge,
                head,
            )
            for witness in self.witnesses[:2]
        ]
        checkpoint = core.make_checkpoint(BINDING, entries, signatures)
        freshness = {
            "schema": rev24.FRESHNESS_SCHEMA,
            "ledgerRevision": REVISION,
            "ledgerSHA256": LEDGER_SHA,
            **BINDING,
            "challengeNonce": self.challenge["challengeNonce"],
            "latestIndex": len(entries) - 1,
            "latestTreeHead": head,
            "observedAtEpoch": NOW,
            "admissionSHA256": core.digest(
                core.canonical(self.admission)
            ),
            "deploymentAcceptanceSHA256": self.deployment_hash,
        }
        freshness_envelope = rev24.sign_authority_statement(
            freshness, self.root
        )
        return entries, checkpoint, freshness_envelope

    def recover(self, entries, checkpoint, freshness):
        return rev24.verify_recovery(
            BINDING,
            entries,
            checkpoint,
            freshness,
            self.admission_envelope,
            self.deployment_envelope,
            self.challenge_envelope,
            core.public_bytes(self.root),
            REVISION,
            LEDGER_SHA,
            NOW,
        )


SUCCESS = (
    "RUN_BOUND",
    "GET_INTENT",
    "GET_SEND_RETURNED",
    "GET_RESPONSE",
    "GET_VALIDATED",
    "PATCH_INTENT",
    "PATCH_SEND_RETURNED",
    "PATCH_RESPONSE",
    "PATCH_VALIDATED_SUCCESS",
    "RUN_TERMINAL",
)


class Rev24ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.fixture = Fixture()

    def test_valid_recovery(self):
        state = self.fixture.recover(*self.fixture.execute(SUCCESS))
        self.assertEqual(state["directAPICallsObserved"], 2)
        self.assertEqual(state["patchApplied"], "YES")

    def test_all_crash_boundaries_recover(self):
        for length in range(1, len(SUCCESS) + 1):
            fixture = Fixture()
            state = fixture.recover(*fixture.execute(SUCCESS[:length]))
            self.assertLessEqual(
                state["directAPICallsPossibleMinimum"],
                state["directAPICallsPossibleMaximum"],
            )

    def test_unsigned_or_forged_admission_rejected(self):
        envelope = copy.deepcopy(self.fixture.admission_envelope)
        envelope["statement"]["trustDomain"] = "attacker.invalid"
        with self.assertRaises(core.EvidenceError):
            rev24.verify_admission(
                envelope,
                core.public_bytes(self.fixture.root),
                REVISION,
                LEDGER_SHA,
            )

    def test_wrong_canonical_root_rejected(self):
        with self.assertRaises(core.EvidenceError):
            rev24.verify_admission(
                self.fixture.admission_envelope,
                core.public_bytes(Ed25519PrivateKey.generate()),
                REVISION,
                LEDGER_SHA,
            )

    def test_cross_role_key_collision_rejected(self):
        envelope = copy.deepcopy(self.fixture.admission_envelope)
        envelope["statement"]["witnessKeys"][0]["publicKeyBase64"] = (
            envelope["statement"]["serviceKeys"][0]["publicKeyBase64"]
        )
        envelope = rev24.sign_authority_statement(
            envelope["statement"], self.fixture.root
        )
        with self.assertRaises(rev24.Rev24Error):
            rev24.verify_admission(
                envelope,
                core.public_bytes(self.fixture.root),
                REVISION,
                LEDGER_SHA,
            )

    def test_exact_three_distinct_witnesses_required(self):
        envelope = copy.deepcopy(self.fixture.admission_envelope)
        envelope["statement"]["witnessKeys"].pop()
        envelope = rev24.sign_authority_statement(
            envelope["statement"], self.fixture.root
        )
        with self.assertRaises(rev24.Rev24Error):
            rev24.verify_admission(
                envelope,
                core.public_bytes(self.fixture.root),
                REVISION,
                LEDGER_SHA,
            )

    def test_challenge_prediction_without_root_signature_rejected(self):
        envelope = copy.deepcopy(self.fixture.challenge_envelope)
        envelope["statement"]["challengeNonce"] = "f" * 64
        with self.assertRaises(core.EvidenceError):
            rev24.verify_challenge(
                envelope,
                core.public_bytes(self.fixture.root),
                REVISION,
                LEDGER_SHA,
                BINDING,
                self.fixture.deployment_hash,
                NOW,
            )

    def test_expired_challenge_rejected(self):
        with self.assertRaises(rev24.Rev24Error):
            rev24.verify_challenge(
                self.fixture.challenge_envelope,
                core.public_bytes(self.fixture.root),
                REVISION,
                LEDGER_SHA,
                BINDING,
                self.fixture.deployment_hash,
                NOW + 101,
            )

    def test_ack_replay_race_rejected(self):
        run_bound = core.make_leaf(
            BINDING, 0, "RUN_BOUND", payload("RUN_BOUND"), core.ZERO_HEAD
        )
        ack0 = self.fixture.service.append(run_bound)
        leaf = core.make_leaf(
            BINDING,
            1,
            "GET_INTENT",
            payload("GET_INTENT"),
            ack0["treeHead"],
        )
        ack = self.fixture.service.append(leaf)
        self.fixture.service.consume_intent_ack(
            leaf,
            ack,
            self.fixture.admission,
            self.fixture.challenge,
            NOW,
        )
        with self.assertRaises(rev24.Rev24Error):
            self.fixture.service.consume_intent_ack(
                leaf,
                ack,
                self.fixture.admission,
                self.fixture.challenge,
                NOW,
            )

    def test_cross_run_ack_rejected(self):
        run_bound = core.make_leaf(
            BINDING, 0, "RUN_BOUND", payload("RUN_BOUND"), core.ZERO_HEAD
        )
        ack0 = self.fixture.service.append(run_bound)
        leaf = core.make_leaf(
            BINDING,
            1,
            "GET_INTENT",
            payload("GET_INTENT"),
            ack0["treeHead"],
        )
        ack = self.fixture.service.append(leaf)
        challenge = copy.deepcopy(self.fixture.challenge)
        challenge["runID"] = "other"
        with self.assertRaises(rev24.Rev24Error):
            self.fixture.service.consume_intent_ack(
                leaf,
                ack,
                self.fixture.admission,
                challenge,
                NOW,
            )

    def test_recovery_requires_ack_consumption_proof(self):
        entries, checkpoint, freshness = self.fixture.execute(SUCCESS)
        entries[1]["intentProof"] = None
        with self.assertRaises(rev24.Rev24Error):
            self.fixture.recover(entries, checkpoint, freshness)

    def test_stale_signed_checkpoint_rejected(self):
        entries, _, _ = self.fixture.execute(SUCCESS)
        stale = entries[:-1]
        head = stale[-1]["treeHead"]
        signatures = [
            witness.sign_log(
                BINDING,
                stale,
                self.fixture.admission,
                self.fixture.challenge,
                head,
            )
            for witness in self.fixture.witnesses[:2]
        ]
        checkpoint = core.make_checkpoint(BINDING, stale, signatures)
        freshness = {
            "schema": rev24.FRESHNESS_SCHEMA,
            "ledgerRevision": REVISION,
            "ledgerSHA256": LEDGER_SHA,
            **BINDING,
            "challengeNonce": self.fixture.challenge["challengeNonce"],
            "latestIndex": len(entries) - 1,
            "latestTreeHead": entries[-1]["treeHead"],
            "observedAtEpoch": NOW,
            "admissionSHA256": core.digest(
                core.canonical(self.fixture.admission)
            ),
            "deploymentAcceptanceSHA256": self.fixture.deployment_hash,
        }
        freshness = rev24.sign_authority_statement(
            freshness, self.fixture.root
        )
        with self.assertRaises(rev24.Rev24Error):
            self.fixture.recover(stale, checkpoint, freshness)

    def test_witness_refuses_arbitrary_head(self):
        entries, _, _ = self.fixture.execute(SUCCESS[:5])
        with self.assertRaises(rev24.Rev24Error):
            self.fixture.witnesses[0].sign_log(
                BINDING,
                entries,
                self.fixture.admission,
                self.fixture.challenge,
                "f" * 64,
            )

    def test_quorum_loss_rejected(self):
        entries, checkpoint, freshness = self.fixture.execute(SUCCESS[:5])
        checkpoint["witnessSignatures"] = checkpoint[
            "witnessSignatures"
        ][:1]
        with self.assertRaises(core.EvidenceError):
            self.fixture.recover(entries, checkpoint, freshness)

    def test_missing_deployment_evidence_rejected(self):
        envelope = copy.deepcopy(self.fixture.deployment_envelope)
        envelope["statement"]["quorumReachable"] = False
        envelope = rev24.sign_authority_statement(
            envelope["statement"], self.fixture.root
        )
        with self.assertRaises(rev24.Rev24Error):
            rev24.verify_deployment(
                envelope,
                core.public_bytes(self.fixture.root),
                REVISION,
                LEDGER_SHA,
                self.fixture.admission,
            )

    def test_malformed_body_counts_rejected(self):
        for count in (True, -1, rev24.MAX_BODY_BYTES + 1):
            fixture = Fixture()
            events = SUCCESS[:4]
            entries, checkpoint, freshness = fixture.execute(events)
            entries[-1]["leaf"]["payload"]["bodyBytes"] = count
            with self.assertRaises(core.EvidenceError):
                fixture.recover(entries, checkpoint, freshness)

    def test_malformed_response_hash_rejected(self):
        entries, checkpoint, freshness = self.fixture.execute(SUCCESS[:4])
        entries[-1]["leaf"]["payload"]["bodySHA256"] = "not-a-hash"
        with self.assertRaises(core.EvidenceError):
            self.fixture.recover(entries, checkpoint, freshness)

    def test_invalid_status_type_and_range_rejected(self):
        for status in (True, "200", 99, 600):
            fixture = Fixture()
            entries, checkpoint, freshness = fixture.execute(SUCCESS[:4])
            entries[-1]["leaf"]["payload"]["status"] = status
            with self.assertRaises(core.EvidenceError):
                fixture.recover(entries, checkpoint, freshness)

    def test_run_bound_is_mandatory(self):
        entries, checkpoint, freshness = self.fixture.execute(SUCCESS[:3])
        with self.assertRaises(rev24.Rev24Error):
            self.fixture.recover(entries[1:], checkpoint, freshness)

    def test_send_ambiguity_preserves_unknown(self):
        events = (
            "RUN_BOUND",
            "GET_INTENT",
            "GET_RESPONSE",
            "GET_VALIDATED",
            "PATCH_INTENT",
            "PATCH_SEND_AMBIGUOUS",
        )
        state = self.fixture.recover(*self.fixture.execute(events))
        self.assertEqual(state["patchApplied"], "UNKNOWN")
        self.assertEqual(len(state["reservationPossibleStates"]), 2)


if __name__ == "__main__":
    unittest.main()
