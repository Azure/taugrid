#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import re
import sys
from pathlib import Path

EXPECTED_IMAGES = {"tau", "taugrid-portal", "tau-core-controller"}
SCHEMA_VERSION = "taugrid.azure.com/acr-build-output/v1"
REGISTRY = "aksairuntime.azurecr.io"


def fail(message: str) -> None:
    raise SystemExit(message)


if len(sys.argv) not in (2, 3):
    fail(f"usage: {Path(sys.argv[0]).name} <images.json> [expected-namespace]")

path = Path(sys.argv[1])
expected_namespace = sys.argv[2] if len(sys.argv) == 3 else None
try:
    document = json.loads(path.read_text())
except (OSError, json.JSONDecodeError) as error:
    fail(f"invalid ACR build output {path}: {error}")

if document.get("schemaVersion") != SCHEMA_VERSION:
    fail(f"expected schemaVersion {SCHEMA_VERSION}")
if document.get("registry") != REGISTRY:
    fail(f"expected registry {REGISTRY}")

images = document.get("images")
if not isinstance(images, list):
    fail("images must be an array")
names = {
    image.get("name")
    for image in images
    if isinstance(image, dict)
}
if names != EXPECTED_IMAGES or len(images) != len(EXPECTED_IMAGES):
    fail("output must contain exactly tau, taugrid-portal, and tau-core-controller")

for image in images:
    name = image["name"]
    tag = image.get("tag")
    digest = image.get("digest")
    reference = image.get("reference")
    if not isinstance(tag, str) or not re.fullmatch(
        r"dev-[0-9]{14}-[0-9a-f]{12}-[0-9]+", tag
    ):
        fail(f"invalid tag for {name}: {tag!r}")
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        fail(f"invalid digest for {name}: {digest!r}")
    namespace = expected_namespace or "pr-"
    expected_reference = f"{REGISTRY}/dev/{namespace}"
    if expected_namespace:
        expected_reference += f"/{name}:"
    if not isinstance(reference, str) or not reference.startswith(expected_reference):
        fail(f"unexpected repository for {name}: {reference!r}")
    if not reference.endswith(f"/{name}:{tag}@{digest}"):
        fail(f"reference fields do not agree for {name}: {reference!r}")

print(f"Validated {path} with all three immutable TauGrid image references.")
