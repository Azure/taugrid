#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import math
import os
import re
import sys


def duration_minutes(name, default, maximum):
    raw = os.environ.get(name) or default
    raw = raw.removeprefix("+")
    parts = list(re.finditer(r"([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(ms|us|µs|μs|ns|h|m|s)", raw))
    if not parts or "".join(p.group() for p in parts) != raw:
        raise ValueError(f"{name} must be a positive duration <= {maximum}m")
    units = {"h": 60, "m": 1, "s": 1 / 60, "ms": 1 / 60000,
             "us": 1 / 60000000, "µs": 1 / 60000000, "μs": 1 / 60000000,
             "ns": 1 / 60000000000}
    minutes = sum(float(p[1]) * units[p[2]] for p in parts)
    if not 1 / 60000000000 <= minutes <= maximum:
        raise ValueError(f"{name} must be a positive duration <= {maximum}m")
    return minutes


def execution_budget(fineweb):
    admission = duration_minutes("CONFORMANCE_ADMISSION_TIMEOUT", "30m", 90)
    completion, capture = 120, 15
    if fineweb:
        completion = int(os.environ.get("FINEWEB_RAYJOB_TIMEOUT_MINUTES") or "120")
        if not 0 < completion <= 180:
            raise ValueError("FINEWEB_RAYJOB_TIMEOUT_MINUTES must be in (0,180]")
        capture = duration_minutes("FINEWEB_CHECKPOINT_CAPTURE_TIMEOUT", "15m", 30)
    minimum = completion + 130 + math.ceil(max(0, admission - 30)) + math.ceil(max(0, capture - 15))
    budget = duration_minutes("CONFORMANCE_GO_TEST_TIMEOUT", "245m", 365) + 5
    if budget < minimum:
        raise ValueError(
            f"CONFORMANCE_GO_TEST_TIMEOUT must be >= {minimum - 5}m; "
            f"caller execution needs >= {minimum}m, including five minutes for cleanup"
        )
    return budget


def capacity(nodes, kind, required, workers):
    if required <= 0 or workers != required:
        raise ValueError("TRAIN_WORKERS must equal positive REQUIRED_GPUS")
    if kind == "fineweb" and required != 16:
        raise ValueError("FineWeb conformance requires 16 GPU workers")
    ready = [
        node for node in nodes["items"]
        if not node.get("spec", {}).get("unschedulable", False)
        and any(c["type"] == "Ready" and c["status"] == "True"
                for c in node.get("status", {}).get("conditions", []))
    ]
    sizes = [int(n["status"].get("allocatable", {}).get("nvidia.com/gpu", 0)) for n in ready]
    sizes = [n for n in sizes if n > 0]
    if sum(sizes) < required:
        raise ValueError(f"structural GPU shortage: Ready schedulable capacity={sum(sizes)}, required={required}")
    if kind == "fineweb" and sorted(sizes) != [8, 8]:
        raise ValueError(f"FineWeb requires exactly two Ready 8-GPU nodes, got {sizes}")
    print(f"Structural capacity={sum(sizes)} GPUs; Kueue owns admission, including temporary reservations.")


def gpu_labels(nodes, instance_type, series):
    if not instance_type or not series:
        raise ValueError("GPU_INSTANCE_TYPE and GPU_SERIES are required")
    matching = []
    for node in nodes["items"]:
        labels = node["metadata"].get("labels", {})
        actual_type = labels.get("node.kubernetes.io/instance-type")
        actual_series = labels.get("kueue.azure.com/gpu-series")
        if actual_type != instance_type and actual_series != series:
            continue
        name = node["metadata"]["name"]
        if actual_type != instance_type or actual_series != series:
            raise ValueError(
                f"Node {name}: expected instance-type={instance_type}, gpu-series={series}; "
                f"got instance-type={actual_type}, gpu-series={actual_series}. "
                "Platform ownership must repair labels; conformance never writes Nodes."
            )
        matching.append(name)
    if not matching:
        raise ValueError(f"No nodes found for instance-type={instance_type}, gpu-series={series}")
    for name in matching:
        print(f"node/{name}")


def ray_image(image, version):
    if not image or re.search(r"\s", image):
        raise ValueError("Ray image must be a non-empty image reference without whitespace")
    match = re.search(r"(?:^|[-:])ray([0-9]+\.[0-9]+\.[0-9]+)(?:[-@]|$)", image)
    inferred = match[1] if match else ""
    version = version or inferred
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("Set RAY_E2E_VERSION to the image's Ray X.Y.Z version when it cannot be inferred")
    if inferred and inferred != version:
        raise ValueError("RAY_E2E_VERSION does not match the Ray version in the image tag")
    return version


def results(lines, selected):
    runs = passes = package_passes = 0
    invalid = False
    for line in lines:
        event = json.loads(line)
        if event.get("Output"):
            print(event["Output"], end="", flush=True)
        action = event.get("Action")
        if action == "fail":
            invalid = True
        if event.get("Test") == selected:
            runs += action == "run"
            passes += action == "pass"
            invalid |= action == "skip"
        if not event.get("Test") and action == "pass":
            package_passes += 1
    if invalid or (runs, passes, package_passes) != (1, 1, 1):
        raise ValueError(
            f"selected test {selected} must run and pass exactly once "
            f"(run={runs}, pass={passes}, package_pass={package_passes}, invalid={invalid}); "
            "skips/missing results are not conformance"
        )


def main():
    if sys.argv[1] == "capacity":
        capacity(json.load(sys.stdin), sys.argv[2], int(sys.argv[3]), int(sys.argv[4]))
    elif sys.argv[1] == "results":
        results(sys.stdin, sys.argv[2])
    elif sys.argv[1] == "budget":
        execution_budget(sys.argv[2] == "fineweb")
        print(os.environ.get("CONFORMANCE_GO_TEST_TIMEOUT") or "245m")
    elif sys.argv[1] == "gpu-labels":
        gpu_labels(json.load(sys.stdin), sys.argv[2], sys.argv[3])
    elif sys.argv[1] == "ray-image":
        print(ray_image(sys.argv[2], sys.argv[3]))
    else:
        raise ValueError("unknown conformance contract")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, TypeError, IndexError, OverflowError) as exc:
        print(f"conformance contract: {exc}", file=sys.stderr)
        sys.exit(1)
