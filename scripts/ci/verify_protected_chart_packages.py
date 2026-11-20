#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import importlib.util
import json
from pathlib import Path
import re
import sys

CHARTS = ("tau-core-controller", "gpu-monitoring", "taugrid-core", "adx-mon", "taugrid")
PRODUCT_CHARTS = ("tau-core-controller", "taugrid-core", "taugrid")
SHA = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"[0-9a-f]{64}")
STABLE = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def load_release_artifacts():
    path = Path(__file__).with_name("release_artifacts.py")
    spec = importlib.util.spec_from_file_location("release_artifacts", path)
    if spec is None or spec.loader is None:
        raise ValueError("release artifact verifier is unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load_release(directory, commit, version):
    if SHA.fullmatch(commit) is None:
        raise ValueError("release source commit must be a full lowercase SHA")
    if STABLE.fullmatch(version) is None:
        raise ValueError("release version must be stable X.Y.Z")
    manifest = json.loads((directory / "release-manifest.json").read_text())
    if set(manifest) != {"commit", "version", "charts", "packages"}:
        raise ValueError("release manifest fields are invalid")
    if manifest["commit"] != commit or manifest["version"] != version:
        raise ValueError("release manifest source or version mismatch")
    if set(manifest["charts"]) != set(CHARTS):
        raise ValueError("release manifest chart set is invalid")
    for name, chart in manifest["charts"].items():
        if set(chart) != {"version", "source_tree"}:
            raise ValueError(f"release manifest chart fields are invalid: {name}")
        if STABLE.fullmatch(str(chart["version"])) is None:
            raise ValueError(f"release manifest chart version is invalid: {name}")
        if SHA.fullmatch(str(chart["source_tree"])) is None:
            raise ValueError(f"release manifest source tree is invalid: {name}")
        if name in PRODUCT_CHARTS and chart["version"] != version:
            raise ValueError(f"release manifest product version mismatch: {name}")
    expected_packages = {
        f"{name}-{chart['version']}.tgz"
        for name, chart in manifest["charts"].items()
    }
    if set(manifest["packages"]) != expected_packages:
        raise ValueError("release manifest package set is invalid")
    if any(
        DIGEST.fullmatch(str(digest)) is None
        for digest in manifest["packages"].values()
    ):
        raise ValueError("release manifest package digest is invalid")
    return {
        "commit": manifest["commit"],
        "version": manifest["version"],
        "charts": manifest["charts"],
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("directory", "archive"))
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--chart", choices=CHARTS)
    parser.add_argument("--archive", type=Path)
    args = parser.parse_args()
    release = load_release(args.directory, args.commit, args.version)
    verifier = load_release_artifacts()
    try:
        if args.command == "directory":
            if args.chart is not None or args.archive is not None:
                raise ValueError("directory verification does not accept an archive")
            verifier.verify_directory(args.directory, release)
        else:
            if args.chart is None or args.archive is None:
                raise ValueError("archive verification requires a chart and archive")
            verifier.verify_chart(args.archive.read_bytes(), args.chart, release)
    except verifier.ReleaseError as error:
        raise ValueError(str(error)) from error


if __name__ == "__main__":
    try:
        main()
    except (
        AttributeError,
        ImportError,
        KeyError,
        OSError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
    ) as error:
        raise SystemExit(f"protected chart packages: {error}") from error
