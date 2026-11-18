# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import importlib
import json
import os
import platform
import sys


def check_image(torch_spec, torch_index_url, ray_version, gpu=False):
    if torch_spec not in ("torch==2.7.1", "torch==2.7.1+cu128"):
        raise RuntimeError(f"unsupported Torch requirement: {torch_spec}")
    if torch_index_url.rstrip("/") != "https://download.pytorch.org/whl/cu128":
        raise RuntimeError(f"unsupported Torch index: {torch_index_url}")
    if ray_version != "2.58.0":
        raise RuntimeError(f"unsupported Ray version: {ray_version}")
    if sys.version_info[:2] != (3, 12):
        raise RuntimeError(f"expected Python 3.12, got {platform.python_version()}")
    if sys.prefix != sys.base_prefix:
        raise RuntimeError("expected globally installed dependencies, not a virtualenv")
    if os.getuid() == 0:
        raise RuntimeError("image contract must run as the default nonroot user")

    torch = importlib.import_module("torch")
    ray = importlib.import_module("ray")
    if torch.__version__ != "2.7.1+cu128" or torch.version.cuda != "12.8":
        raise RuntimeError(
            f"expected Torch 2.7.1+cu128/CUDA 12.8, "
            f"got {torch.__version__}/CUDA {torch.version.cuda}"
        )
    if ray.__version__ != ray_version:
        raise RuntimeError(f"expected Ray {ray_version}, got {ray.__version__}")

    result = {
        "python": platform.python_version(),
        "ray": ray.__version__,
        "torch": torch.__version__,
        "cuda": torch.version.cuda,
        "torch_file": os.path.realpath(torch.__file__),
        "uid": os.getuid(),
    }
    if gpu:
        if not torch.cuda.is_available() or torch.cuda.device_count() == 0:
            raise RuntimeError("expected at least one allocated CUDA device")
        checksums = []
        for device in range(torch.cuda.device_count()):
            left = torch.ones((256, 256), device=f"cuda:{device}")
            checksum = float((left @ left).sum().item())
            if checksum != 16777216.0:
                raise RuntimeError(f"CUDA device {device} returned checksum {checksum}")
            checksums.append({"device": device, "checksum": checksum})
        result["gpu_checksums"] = checksums
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--torch-spec", default="torch==2.7.1")
    parser.add_argument(
        "--torch-index-url", default="https://download.pytorch.org/whl/cu128"
    )
    parser.add_argument("--ray-version", default="2.58.0")
    parser.add_argument("--gpu", action="store_true")
    args = parser.parse_args()
    try:
        result = check_image(
            args.torch_spec, args.torch_index_url, args.ray_version, args.gpu
        )
    except (ImportError, RuntimeError) as error:
        parser.exit(1, f"Ray Torch image contract failed: {error}\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
