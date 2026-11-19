# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import importlib
import os
import platform
import sys


def check_python_ray(ray_version):
    if ray_version != "2.58.0":
        raise RuntimeError(f"unsupported Ray version: {ray_version}")
    if sys.version_info[:2] != (3, 12):
        raise RuntimeError(f"expected Python 3.12, got {platform.python_version()}")
    if os.getuid() == 0:
        raise RuntimeError("matrix must run as the default nonroot user")
    ray = importlib.import_module("ray")
    if ray.__version__ != ray_version:
        raise RuntimeError(f"expected Ray {ray_version}, got {ray.__version__}")


def check_base(ray_version):
    if sys.prefix != sys.base_prefix:
        raise RuntimeError("Ray base image must use the global interpreter")
    check_python_ray(ray_version)


def check_runtime():
    torch_spec = os.environ["MATRIX_TORCH_SPEC"]
    torch_index = os.environ["MATRIX_TORCH_INDEX_URL"]
    if torch_spec not in ("torch==2.7.1", "torch==2.7.1+cu128"):
        raise RuntimeError(f"unsupported Torch requirement: {torch_spec}")
    if torch_index.rstrip("/") != "https://download.pytorch.org/whl/cu128":
        raise RuntimeError(f"unsupported Torch index: {torch_index}")
    check_python_ray(os.environ["MATRIX_RAY_VERSION"])
    torch = importlib.import_module("torch")
    if torch.__version__ != "2.7.1+cu128" or torch.version.cuda != "12.8":
        raise RuntimeError(
            f"expected Torch 2.7.1+cu128/CUDA 12.8, "
            f"got {torch.__version__}/CUDA {torch.version.cuda}"
        )


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--ray-version", required=True)
    args = parser.parse_args()
    check_base(args.ray_version)
