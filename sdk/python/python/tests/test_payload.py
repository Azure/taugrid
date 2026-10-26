# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Cross-platform determinism tests for the embedded-payload transport."""

from __future__ import annotations

import base64
import hashlib
import json
from typing import Mapping

from tau._notebook_pkg import package
from tau._payload import decode

NOTEBOOK_BYTES = json.dumps(
    {
        "nbformat": 4,
        "nbformat_minor": 5,
        "metadata": {
            "kernelspec": {"language": "python", "name": "python3"},
            "language_info": {"name": "python"},
        },
        "cells": [
            {"cell_type": "code", "id": "c0", "metadata": {}, "outputs": [{"x": 1}], "execution_count": 3, "source": ["print(1)"]},
        ],
    }
).encode()
PINNED_ENCODED = """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"""
PINNED_DIGEST = "f880a04c592cb05916f68f8fb98a48ebca880351319452b523665dcc2e8393de"
PINNED_SHA256 = "b7faeed9c82e04dfea154b30169dc055890b229d82fd62f87779321faaa5f9bb"


def _pseudo_random(length: int) -> bytes:
    state = 20261026
    out = bytearray()
    for _ in range(length):
        state = (1103515245 * state + 12345) & 0x7FFFFFFF
        out.append(state >> 16 & 0xFF)
    return bytes(out)


INCOMPRESSIBLE = _pseudo_random(12000)


def _envelope(files: Mapping[str, bytes]) -> bytes:
    return json.dumps(
        {
            "version": 2,
            "files": [{"name": name, "data": base64.b64encode(files[name]).decode("ascii")} for name in sorted(files)],
        },
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")


def test_encode_matches_the_pinned_cross_platform_bytes():
    staged = package(NOTEBOOK_BYTES)
    assert staged.encoding.encoded == PINNED_ENCODED
    assert hashlib.sha256(staged.encoding.encoded.encode()).hexdigest() == PINNED_SHA256


def test_decode_round_trips_the_pinned_cross_platform_bytes():
    staged = package(NOTEBOOK_BYTES)
    assert decode(PINNED_ENCODED, PINNED_DIGEST) == staged.files


def test_digest_is_sha256_of_the_uncompressed_envelope():
    staged = package(NOTEBOOK_BYTES)
    assert staged.encoding.digest == PINNED_DIGEST
    assert staged.encoding.digest == hashlib.sha256(_envelope(staged.files)).hexdigest()
