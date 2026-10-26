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
PINNED_DIGEST = "591098cb0d07efdb3eb14c0bc09af920c23c016a527659e8afbdd4cfb17bb2d8"
PINNED_SHA256 = "9d5b76099d24b1c518df52e4e2801bb11fed406d5fb1a463776f9913d337062e"


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
