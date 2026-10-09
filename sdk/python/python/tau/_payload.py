# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Embedded-payload transport for the notebook plugin (CLI parity).

The Go CLI renders self-contained workloads: generated files are embedded in the
workload spec as a gzip+base64 envelope on a tau-payload initContainer, not a
per-run ConfigMap and not an object-store upload. MultiKueue mirrors the workload
object but not auxiliary objects, so embedding is what keeps the run portable
across worker clusters.

This module is the Python producer/consumer for that transport. It mirrors the
CLI contract where it matters:

* envelope version 2 with canonically ordered flat files,
* gzip then base64, SHA-256 over the transported (pre-base64) gzip bytes,
* the same env var names, initContainer identity, target directory and volume,
* the same binding cap: len(TAU_PAYLOAD_B64) + 1 + len(encoded) <= 64 KiB
  (no terminating NUL in this accounting),
* the same decoded ceiling: 1 MiB of raw file content.

Go and Python gzip framing differ, so the encoders are format-compatible, not
byte-identical. Each producer computes the digest over its own transported bytes
and each consumer verifies it, which is what the initContainer relies on.
"""

from __future__ import annotations

import base64
import gzip
import hashlib
import json
from dataclasses import dataclass
from typing import Dict, Mapping

#: Binding cap on the whole TAU_PAYLOAD_B64=<...> environment entry.
MAX_ENV_ENTRY_BYTES = 64 * 1024
#: Sanity ceiling on the sum of raw file bytes before encoding.
MAX_DECODED_BYTES = 1024 * 1024

ENVELOPE_VERSION = 2

ANNOTATION_DIGEST = "tau.azure.com/payload-digest"
INIT_CONTAINER_NAME = "tau-payload"
ENV_B64 = "TAU_PAYLOAD_B64"
ENV_DIGEST = "TAU_PAYLOAD_DIGEST"
ENV_TARGET_DIR = "TAU_PAYLOAD_TARGET_DIR"
#: Where the initContainer materialises the payload; the entrypoint runs from here.
TARGET_DIR = "/script"
#: The shared emptyDir volume the initContainer writes and the container mounts.
VOLUME_NAME = "script"


class PayloadError(Exception):
    """Base class for payload transport failures."""


class PayloadTooLarge(PayloadError):
    """The encoded payload cannot fit a single environment entry."""


class PayloadInvalid(PayloadError):
    """The file map or transported envelope is malformed."""


@dataclass(frozen=True)
class EncodedPayload:
    """An immutable encoding result: one snapshot of files plus its transport."""

    files: Mapping[str, bytes]
    encoded: str
    digest: str
    decoded_bytes: int
    compressed_bytes: int
    env_entry_bytes: int


#: Mirrors payload.InitContainerScript at the reviewed CLI version: decode the
#: base64 gzip envelope, verify the SHA-256 digest, and write flat files to
#: TAU_PAYLOAD_TARGET_DIR. Stdlib only, so it can reuse the workload's image.
INIT_CONTAINER_SCRIPT = '''import base64
import gzip
import hashlib
import json
import os
import sys

raw_b64 = os.environ["TAU_PAYLOAD_B64"]
want_digest = os.environ["TAU_PAYLOAD_DIGEST"]
target_dir = os.environ["TAU_PAYLOAD_TARGET_DIR"]

raw = base64.b64decode(raw_b64)
got_digest = hashlib.sha256(raw).hexdigest()
if got_digest != want_digest:
    sys.stderr.write("tau-payload: integrity check failed: digest=%s want=%s\\n" % (got_digest, want_digest))
    sys.exit(1)

raw = gzip.decompress(raw)
envelope = json.loads(raw)
if envelope.get("version") != 2:
    sys.stderr.write("tau-payload: unsupported envelope version %r\\n" % (envelope.get("version"),))
    sys.exit(1)
files = envelope.get("files", [])
os.makedirs(target_dir, exist_ok=True)
for f in files:
    name = f["name"]
    if not name or name in (".", "..") or "/" in name or "\\\\" in name:
        sys.stderr.write("tau-payload: rejecting unsafe file name %r\\n" % (name,))
        sys.exit(1)
    path = os.path.join(target_dir, name)
    with open(path, "wb") as fh:
        fh.write(base64.b64decode(f["data"]))
    os.chmod(path, 0o644)

sys.stderr.write("tau-payload: wrote %d file(s) to %s\\n" % (len(files), target_dir))
'''


def validate_files(files: Mapping[str, bytes]) -> Dict[str, bytes]:
    """Return a copy after checking flat, unique, non-empty file identities."""
    if not isinstance(files, Mapping) or not files:
        raise PayloadInvalid("payload must contain at least one file")
    checked: Dict[str, bytes] = {}
    for name, data in files.items():
        if not isinstance(name, str) or not name:
            raise PayloadInvalid(f"payload file name must be a non-empty string, got {name!r}")
        if name in (".", "..") or "/" in name or "\\" in name:
            raise PayloadInvalid(f"payload file name must be a flat name without path separators: {name!r}")
        if not isinstance(data, (bytes, bytearray)):
            raise PayloadInvalid(f"payload file {name!r} must be bytes, got {type(data).__name__}")
        checked[name] = bytes(data)
    return checked


def encode(files: Mapping[str, bytes]) -> EncodedPayload:
    """Encode a file map into the CLI transport envelope.

    Validates names and types first, then enforces the decoded ceiling and the
    binding encoded-entry cap with actionable wording.
    """
    checked = validate_files(files)
    decoded_bytes = sum(len(data) for data in checked.values())
    if decoded_bytes > MAX_DECODED_BYTES:
        raise PayloadTooLarge(_size_error("generated payload", decoded_bytes, MAX_DECODED_BYTES))

    envelope = {
        "version": ENVELOPE_VERSION,
        "files": [
            {"name": name, "data": base64.b64encode(checked[name]).decode("ascii")}
            for name in sorted(checked)
        ],
    }
    raw = json.dumps(envelope, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    compressed = gzip.compress(raw, compresslevel=9, mtime=0)
    encoded = base64.b64encode(compressed).decode("ascii")
    env_entry_bytes = len(ENV_B64) + 1 + len(encoded)
    if env_entry_bytes > MAX_ENV_ENTRY_BYTES:
        raise PayloadTooLarge(_size_error("encoded payload environment entry", env_entry_bytes, MAX_ENV_ENTRY_BYTES))

    return EncodedPayload(
        files=checked,
        encoded=encoded,
        digest=hashlib.sha256(compressed).hexdigest(),
        decoded_bytes=decoded_bytes,
        compressed_bytes=len(compressed),
        env_entry_bytes=env_entry_bytes,
    )


def decode(encoded: str, digest: str) -> Dict[str, bytes]:
    """Reverse encode(), verifying the digest and envelope version."""
    try:
        transported = base64.b64decode(encoded, validate=True)
    except Exception as exc:
        raise PayloadInvalid(f"payload is not valid base64: {exc}") from exc
    got = hashlib.sha256(transported).hexdigest()
    if got != digest:
        raise PayloadInvalid(f"payload integrity check failed: digest={got} want={digest}")
    try:
        envelope = json.loads(gzip.decompress(transported).decode("utf-8"))
    except Exception as exc:
        raise PayloadInvalid(f"payload envelope is not decodable: {exc}") from exc
    if envelope.get("version") != ENVELOPE_VERSION:
        raise PayloadInvalid(f"unsupported envelope version {envelope.get('version')!r}")
    files: Dict[str, bytes] = {}
    for entry in envelope.get("files", []):
        name = entry.get("name")
        if name in files:
            raise PayloadInvalid(f"duplicate payload file name {name!r}")
        files[name] = base64.b64decode(entry.get("data", ""))
    return validate_files(files)


def _size_error(what: str, actual: int, limit: int) -> str:
    return (
        f"{what} is {actual} bytes, which exceeds the limit of {limit} bytes ({limit // 1024} KiB) "
        f"by {actual - limit} bytes; payloads are gzip-compressed and embedded directly in the "
        "workload spec so the run stays self-contained. Remedies: split rarely-changing code out "
        "of the entrypoint and bake it into a certified Ray image, or mount large assets from a "
        "pre-provisioned PVC, instead of embedding them in the workload spec"
    )


__all__ = [
    "encode",
    "decode",
    "validate_files",
    "EncodedPayload",
    "PayloadError",
    "PayloadTooLarge",
    "PayloadInvalid",
    "MAX_ENV_ENTRY_BYTES",
    "MAX_DECODED_BYTES",
    "ENVELOPE_VERSION",
    "ANNOTATION_DIGEST",
    "INIT_CONTAINER_NAME",
    "ENV_B64",
    "ENV_DIGEST",
    "ENV_TARGET_DIR",
    "TARGET_DIR",
    "VOLUME_NAME",
    "INIT_CONTAINER_SCRIPT",
]
