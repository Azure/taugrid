#!/usr/bin/env python3
import gzip
import hashlib
import io
import json
import os
import stat
import subprocess
import tarfile
from pathlib import Path


ROOT = Path(__file__).resolve().parent
ARCHIVE = ROOT.with_suffix(".tar.gz")
LEDGER = Path(
    "/Users/chokevin/.copilot/session-state/"
    "196838f3-ee4f-4c89-8d0c-b72207ba59dc/files"
)
VERIFY = Path(
    "/Users/chokevin/dev/copilot-worktrees/unbounded/"
    "chokevin-studious-system/tmp/"
    "r050708-guarded-bootstrap-contract-rev23-independent-verification-20261019"
)
REV23 = Path(
    "tmp/r050708-guarded-bootstrap-contract-rev23-20261019t232300z.tar.gz"
).resolve()
EXPECTED = {
    "ledger-json": (
        LEDGER / "certification-evidence-ledger.json",
        "a085081217ec47fcd0882bde8af4498bbc21a995849502f86015bbd53f5ae01c",
        1599833,
    ),
    "ledger-markdown": (
        LEDGER / "certification-evidence-ledger.md",
        "707ab5a70d4d9b78fa2994bcf14564a583db4c2bdbfc710b22051f4ee6a0b457",
        322274,
    ),
    "ledger-manifest": (
        LEDGER / "CHECKSUMS.sha256",
        "a876c51717bbb5d16c7a3383b02d0c3d1f4b9070f8e63776664a8fd98a2b25ab",
        532,
    ),
    "rejection": (
        VERIFY / "REV23-REJECTION.json",
        "54f648a4ef255713591c42bdead998b132c4bb84993b3a1161b3cea7513abe5a",
        4954,
    ),
    "adversarial": (
        VERIFY / "independent-adversarial-results.json",
        "cc720ac2f456ab63af772eb690935832a1f2e46141a14d8db8b94e1e174b68e9",
        5690,
    ),
    "verifier": (
        VERIFY / "verify_rev23.py",
        "e60213137bb99444780f3146944ee4f2f430147f9fc158b57c5558a1c3f1caf3",
        23323,
    ),
    "rev23-archive": (
        REV23,
        "5533ce9a07c70d4d737382c9d220938cd05ca5321957eb7f14e25697e567dd10",
        3988028,
    ),
}


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encoded(value):
    return (
        json.dumps(value, sort_keys=True, indent=2, ensure_ascii=True) + "\n"
    ).encode()


def write_once(relative, data):
    path = ROOT / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o444)
    with os.fdopen(descriptor, "wb") as output:
        output.write(data)
    return path


def archive_bytes():
    output = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w") as archive:
            for path in sorted(p for p in ROOT.rglob("*") if p.is_file()):
                if "__pycache__" in path.parts:
                    continue
                data = path.read_bytes()
                info = tarfile.TarInfo(
                    f"{ROOT.name}/{path.relative_to(ROOT).as_posix()}"
                )
                info.size = len(data)
                info.mode = 0o444
                info.mtime = info.uid = info.gid = 0
                info.uname = info.gname = ""
                archive.addfile(info, io.BytesIO(data))
    return output.getvalue()


def main():
    if ARCHIVE.exists():
        raise RuntimeError("archive already exists")
    inputs = {}
    for role, (path, expected_sha, expected_bytes) in EXPECTED.items():
        data = path.read_bytes()
        if sha(data) != expected_sha or len(data) != expected_bytes:
            raise RuntimeError(f"immutable provenance mismatch: {role}")
        inputs[role] = data
    completed = subprocess.run(
        ["python3", "-m", "unittest", "discover", "-s", "tests", "-v"],
        cwd=ROOT,
        env={**os.environ, "PYTHONPATH": str(ROOT)},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    if (
        completed.returncode != 0
        or b"Ran 78 tests" not in completed.stdout
        or not completed.stdout.rstrip().endswith(b"OK")
    ):
        raise RuntimeError(completed.stdout.decode(errors="replace"))
    write_once("validation/TEST-OUTPUT.txt", completed.stdout)
    contract = {
        "schema": "dapple-r050708-guarded-bootstrap-contract-rev24/v1",
        "verdict": "OFFLINE_REV24_DIAGNOSTIC_CONTRACT_CANDIDATE",
        "closes": {
            "ledgerRevision": 354,
            "rejection": (
                "REJECT_OFFLINE_REV23_UNANCHORED_ADMISSION_"
                "REPLAYABLE_ACK_AND_STALE_CHECKPOINT"
            ),
            "blockers": [
                "unsigned caller-supplied admission",
                "witness role, identity, and exact quorum defects",
                "ACK replay without authority freshness",
                "signed stale checkpoint rollback",
                "recovery lacking service ACK proof",
                "incomplete event and response closure",
                "missing deployment and availability prerequisite",
            ],
        },
        "canonicalAuthorityAnchor": {
            "required": True,
            "admissionSignature": "Ed25519 by already-canonical authority root",
            "binds": [
                "exact ledger revision and JSON SHA256",
                "trust domain and algorithm",
                "one service identity, role, key ID, and public key",
                "exactly three witness identities, roles, key IDs, and distinct public keys",
                "exact 2-of-3 threshold",
                "validity, rotation, and revocation state",
                "separately accepted deployment acceptance ID",
            ],
            "producerSelfAdmission": False,
            "crossRoleOrWithinRoleKeyCollision": "rejected",
        },
        "deploymentPrerequisite": {
            "separateAcceptanceRequired": True,
            "authoritySigned": True,
            "closedEvidence": [
                "service identity and endpoint",
                "three distinct witness identities and endpoints",
                "key custody evidence SHA256",
                "quorum reachability",
                "durable persistence and retention lock",
                "verified backup",
                "freshness and gossip health",
                "exact admission SHA256 match",
            ],
            "missingOrUnhealthy": "no future live authority",
            "deployedByThisPacket": False,
        },
        "freshOneTimePreSendAck": {
            "authorityChallenge": [
                "canonical-root signature",
                "unpredictable 256-bit nonce",
                "authority/run/transfer/contract/generation binding",
                "issued and bounded expiry epochs",
                "prior accepted checkpoint index and head",
                "deployment acceptance SHA256",
            ],
            "sequence": [
                "durably append exact request INTENT",
                "verify service-signed ACK bound to challenge and log head",
                "atomically consume ACK once at service",
                "receive signed consumption proof",
                "only then authorize API send",
            ],
            "replay": (
                "same nonce/run/sequence/event/request tuple cannot be consumed twice; "
                "old, expired, cross-run, cross-request, or post-hoc proof is rejected"
            ),
            "recoveryRequires": "both original ACK and signed consumption proof",
        },
        "antiStaleAndConsistency": {
            "authorityExpectedLatest": (
                "recovery requires canonical-root-signed latest checkpoint "
                "index and tree head observed through witness gossip"
            ),
            "priorConsistency": (
                "challenge binds prior accepted checkpoint; recovered complete "
                "log must reproduce that head at that exact index"
            ),
            "latestConsistency": (
                "entries, witnessed checkpoint, and authority freshness "
                "statement must agree exactly on latest index/head"
            ),
            "rejects": [
                "stale signed prefix",
                "withheld suffix",
                "rollback or divergent fork",
                "split view and witness equivocation",
                "reorder, replay, gap, or substitution",
            ],
            "availabilityAssumption": (
                "at least two of three admitted independent witnesses and "
                "authority freshness gossip are reachable and non-equivocating"
            ),
        },
        "witnessValidation": {
            "blindHeadSigning": False,
            "beforeSigning": [
                "validate full log from zero head",
                "validate mandatory RUN_BOUND",
                "validate every transition and canonical payload",
                "validate service ACK and one-time consumption proof on every INTENT",
                "validate request hashes, previous heads, leaf hashes, and tree heads",
                "validate prior-checkpoint consistency",
            ],
        },
        "closedEvidenceSemantics": {
            "hashes": "exact lowercase 64-character hexadecimal SHA256",
            "bodyBytes": "non-boolean integer 0 through 16777216",
            "status": "non-boolean integer 100 through 599",
            "reason": "bounded string",
            "runBound": "mandatory first event with exact authority/transfer/contract",
            "requestCrossBinding": (
                "exact GET or PATCH request SHA256 carried through intent, "
                "ACK, consumption, send disposition, response, and validation"
            ),
            "honestUnknown": (
                "send ambiguity and PATCH application remain UNKNOWN unless "
                "validated response evidence proves a terminal classification"
            ),
        },
        "preservedRev21Controls": {
            "exactDirectHTTPSWireContract": True,
            "strictDuplicateAndNonFiniteRejectingJSON": True,
            "closedNodeAndStatusProjections": True,
            "completeBoundedResponseEvidence": True,
            "secretSafeLifecycle": True,
            "singleNearWriteGETConditionalAtomicFullSpecPATCH": True,
            "noRetryFallbackExtraOrPostStateGET": True,
            "terminalOneUseTransferConsumption": True,
        },
        "canonicalCurrentState": {
            "directAPICalls": {"minimum": 0, "maximum": 2},
            "patchMutations": {"minimum": 0, "maximum": 1},
            "reservationObserved": None,
            "reservationPossible": [
                "rows05-08-remediation-20261019t003520z-v2",
                "rows05-08-remediation-20261019t080808z-v3",
            ],
            "rev347AuthorityAndTransfer": "terminal_non_reusable",
        },
        "authority": {
            "live": False,
            "network": False,
            "recovery": False,
            "stateRead": False,
            "mutation": False,
            "retry": False,
            "replacementTransfer": False,
            "patch": False,
            "rows": False,
            "cleanup": False,
            "release": False,
            "continuation": False,
            "row15": False,
        },
    }
    write_once("CONTRACT.json", encoded(contract))
    write_once(
        "README.md",
        (
            "# Rev24 canonically anchored evidence contract candidate\n\n"
            "Offline-only remediation of rev23. Trust admission, deployment "
            "acceptance, unpredictable run challenge, and expected-latest "
            "freshness are signed by an already-canonical authority root. "
            "Every API send requires a fresh service ACK plus one-time signed "
            "consumption proof. Witnesses validate the complete log before "
            "signing, and recovery must match the authority-provided latest "
            "checkpoint. This packet deploys nothing and grants no authority.\n"
        ).encode(),
    )
    provenance = {
        "provenance/LEDGER-REV354.json": inputs["ledger-json"],
        "provenance/LEDGER-REV354.md": inputs["ledger-markdown"],
        "provenance/LEDGER-REV354-CHECKSUMS.sha256": inputs["ledger-manifest"],
        "provenance/REV23-REJECTION.json": inputs["rejection"],
        "provenance/REV23-ADVERSARIAL.json": inputs["adversarial"],
        "provenance/REV23-VERIFIER.py": inputs["verifier"],
        "provenance/REV23-CANDIDATE.tar.gz": inputs["rev23-archive"],
    }
    for relative, data in provenance.items():
        write_once(relative, data)
    validation = {
        "schema": "dapple-r050708-guarded-bootstrap-rev24-validation/v1",
        "outcome": "PASS_OFFLINE_CONTRACT_CANDIDATE",
        "tests": 78,
        "coverage": {
            "allAcceptedRev21Tests": True,
            "canonicalRootSignedAdmission": True,
            "forgedOrUnsignedAdmissionRejected": True,
            "exactDistinctServiceAndWitnessRoles": True,
            "crossRoleAndWithinRoleKeyCollisionRejected": True,
            "freshUnpredictableChallengeRequired": True,
            "expiredCrossRunAndReplayRaceAckRejected": True,
            "ackAndConsumptionProofRequiredForRecovery": True,
            "authorityExpectedLatestRejectsStaleSignedPrefix": True,
            "witnessValidatesFullLogAndRefusesArbitraryHead": True,
            "quorumLossAndDeploymentUnavailabilityRejected": True,
            "mandatoryRunBoundAndClosedHashesCountsStatus": True,
            "allModeledCrashBoundariesAndHonestUnknown": True,
        },
        "zeroLiveNetworkCalls": True,
        "zeroRecoveryCalls": True,
        "zeroStateReads": True,
        "zeroMutations": True,
    }
    write_once("validation/VALIDATION.json", encoded(validation))
    for path in (p for p in ROOT.rglob("*") if p.is_file()):
        if "__pycache__" not in path.parts:
            os.chmod(path, 0o444)
    members = []
    for path in sorted(p for p in ROOT.rglob("*") if p.is_file()):
        if "__pycache__" in path.parts or path.name == "EVIDENCE-INDEX.json":
            continue
        data = path.read_bytes()
        members.append(
            {
                "path": path.relative_to(ROOT).as_posix(),
                "sha256": sha(data),
                "bytes": len(data),
                "mode": f"{stat.S_IMODE(path.stat().st_mode):04o}",
            }
        )
    index_path = write_once(
        "EVIDENCE-INDEX.json",
        encoded(
            {
                "schema": "dapple-r050708-guarded-bootstrap-rev24-index/v1",
                "verdict": contract["verdict"],
                "memberCount": len(members),
                "members": members,
                "zeroLiveNetworkCalls": True,
                "zeroRecoveryCalls": True,
                "zeroStateReads": True,
                "zeroMutations": True,
                "row15Prohibited": True,
            }
        ),
    )
    receipt_path = write_once(
        "PACKET-RECEIPT.json",
        encoded(
            {
                "schema": "dapple-r050708-guarded-bootstrap-rev24-receipt/v1",
                "verdict": contract["verdict"],
                "contractSHA256": sha((ROOT / "CONTRACT.json").read_bytes()),
                "validationSHA256": sha(
                    (ROOT / "validation/VALIDATION.json").read_bytes()
                ),
                "indexSHA256": sha(index_path.read_bytes()),
                "tests": 78,
                "mode": "0444",
                "independentVerificationRequired": True,
                "serviceDeployed": False,
                "liveAuthority": False,
                "recoveryAuthority": False,
                "replacementTransferIssued": False,
                "row15": False,
            }
        ),
    )
    archive = archive_bytes()
    if archive != archive_bytes():
        raise RuntimeError("deterministic archive reseal mismatch")
    descriptor = os.open(ARCHIVE, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o444)
    with os.fdopen(descriptor, "wb") as output:
        output.write(archive)
    os.chmod(ARCHIVE, 0o444)
    print(
        json.dumps(
            {
                "schema": "dapple-r050708-guarded-bootstrap-rev24-sealed/v1",
                "verdict": contract["verdict"],
                "root": str(ROOT),
                "archive": str(ARCHIVE),
                "archiveSHA256": sha(archive),
                "archiveBytes": len(archive),
                "archiveMode": "0444",
                "archiveMemberCount": len(
                    [
                        path
                        for path in ROOT.rglob("*")
                        if path.is_file() and "__pycache__" not in path.parts
                    ]
                ),
                "contractSHA256": sha((ROOT / "CONTRACT.json").read_bytes()),
                "contractBytes": (ROOT / "CONTRACT.json").stat().st_size,
                "validationSHA256": sha(
                    (ROOT / "validation/VALIDATION.json").read_bytes()
                ),
                "validationBytes": (
                    ROOT / "validation/VALIDATION.json"
                ).stat().st_size,
                "indexSHA256": sha(index_path.read_bytes()),
                "indexBytes": index_path.stat().st_size,
                "receiptSHA256": sha(receipt_path.read_bytes()),
                "receiptBytes": receipt_path.stat().st_size,
                "tests": 78,
                "zeroLiveNetworkCalls": True,
                "zeroRecoveryCalls": True,
                "zeroStateReads": True,
                "zeroMutations": True,
                "independentVerificationRequired": True,
                "row15Prohibited": True,
            },
            sort_keys=True,
            indent=2,
        )
    )


if __name__ == "__main__":
    main()
