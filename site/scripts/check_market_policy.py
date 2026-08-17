#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Validate the checked-in H200 market-policy artifact and its provenance."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

EXPECTED_SHA256 = "d1acef1b21999e8e08cb893ff24f3c3a4d384bd5a3c28d8afe0bdeefe2547923"
EXPECTED_ARCHITECTURE = {
    "actionCount": 3,
    "activation": "relu",
    "hiddenCount": 24,
    "inputCount": 8,
    "policyHead": "softmax",
    "valueHead": "linear",
}


def fail(message: str) -> None:
    raise SystemExit(f"market policy check failed: {message}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("artifact", type=Path)
    args = parser.parse_args()

    content = args.artifact.read_bytes()
    digest = hashlib.sha256(content).hexdigest()
    if digest != EXPECTED_SHA256:
        fail(f"sha256 {digest}, expected H200 artifact {EXPECTED_SHA256}")

    artifact = json.loads(content)
    if artifact.get("format") != "taugrid-market-policy" or artifact.get("version") != 1:
        fail("unsupported format or version")
    if artifact.get("architecture") != EXPECTED_ARCHITECTURE:
        fail("architecture does not match the browser Worker contract")

    training = artifact.get("training", {})
    if training.get("device") != "cuda" or training.get("gpuName") != "NVIDIA H200":
        fail("artifact provenance is not the verified NVIDIA H200 CUDA run")

    weights = artifact.get("weights", {})
    parameter_count = sum(len(values) for values in weights.values())
    if parameter_count != 316:
        fail(f"parameter count {parameter_count}, expected 316")

    metrics = artifact.get("metrics", {})
    if metrics.get("policyAccuracy", 0) < 0.98:
        fail("held-out policy accuracy is below 98%")
    if metrics.get("valueRmse", float("inf")) > 0.04:
        fail("held-out value RMSE exceeds 0.04")

    print(
        f"Validated H200 market policy: sha256={digest} "
        f"parameters={parameter_count}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
