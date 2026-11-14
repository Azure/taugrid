# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import concurrent.futures
import json
import os
from pathlib import Path
import signal
import tempfile
import threading

MAX_WRITABLE_BYTES = 512 * 1024 * 1024


def package_info():
    import os
    from pathlib import Path
    import sys
    import ray
    import torch

    config = Path(sys.prefix) / "pyvenv.cfg"
    system_site_packages = config.is_file() and any(
        line.partition("=")[0].strip() == "include-system-site-packages"
        and line.partition("=")[2].strip().lower() == "true"
        for line in config.read_text().splitlines()
    )
    return {
        "torch_file": os.path.realpath(torch.__file__),
        "torch": torch.__version__,
        "cuda": torch.version.cuda,
        "ray": ray.__version__,
        "global_interpreter": sys.prefix == sys.base_prefix,
        "system_site_packages": system_site_packages,
    }


def allocated_bytes(root):
    total = 0
    for path in root.rglob("*"):
        try:
            total += path.lstat().st_blocks * 512
        except FileNotFoundError:
            continue
    return total


def track_growth(root, stop):
    peak = 0
    while True:
        peak = max(peak, allocated_bytes(root))
        if stop.wait(0.1):
            return peak


def main():
    import ray
    from ray import serve

    signal.alarm(360)
    pip_spec = json.loads(Path(__file__).with_name("pip_spec.json").read_text())
    base = package_info()
    if (
        base["torch"] != "2.7.1+cu128"
        or base["cuda"] != "12.8"
        or not base["global_interpreter"]
    ):
        raise RuntimeError(f"unexpected global packages: {base}")
    observed = {}
    with tempfile.TemporaryDirectory(prefix="ray-torch-contract-") as directory:
        root = Path(directory)
        for name in ("scratch", "home", "cache"):
            (root / name).mkdir()
        os.environ["TMPDIR"] = str(root / "scratch")
        os.environ["HOME"] = str(root / "home")
        os.environ["XDG_CACHE_HOME"] = str(root / "cache")
        tempfile.tempdir = str(root / "scratch")
        stop = threading.Event()
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            growth = executor.submit(track_growth, root, stop)
            try:
                ray.init(
                    num_cpus=2,
                    include_dashboard=False,
                    object_store_memory=80 * 1024 * 1024,
                    _node_ip_address="127.0.0.1",
                    _temp_dir=str(root / "ray"),
                    runtime_env={
                        "pip": pip_spec,
                        "env_vars": {"NCCL_IB_DISABLE": "1"},
                        "config": {"setup_timeout_seconds": 120},
                    },
                )
                observed["job"] = ray.get(
                    ray.remote(num_cpus=0.1)(package_info).remote(), timeout=120
                )

                @serve.deployment(
                    ray_actor_options={
                        "num_cpus": 0.1,
                        "runtime_env": {
                            "pip": pip_spec,
                            "config": {"setup_timeout_seconds": 120},
                        },
                    }
                )
                class PackageProbe:
                    def inspect(self):
                        return package_info()

                handle = serve.run(PackageProbe.bind(), route_prefix=None)
                observed["serve"] = handle.inspect.remote().result(timeout_s=120)
                for context, info in observed.items():
                    if (
                        any(info[key] != base[key] for key in ("torch_file", "torch", "cuda", "ray"))
                        or info["global_interpreter"]
                        or not info["system_site_packages"]
                    ):
                        raise RuntimeError(
                            f"{context} runtime env did not reuse global packages: {info}"
                        )
                if list(root.rglob("site-packages/torch/__init__.py")):
                    raise RuntimeError("runtime environment copied Torch")
            finally:
                try:
                    if ray.is_initialized():
                        serve.shutdown()
                        ray.shutdown()
                finally:
                    stop.set()
                    peak = growth.result()
        if peak > MAX_WRITABLE_BYTES:
            raise RuntimeError(
                f"runtime environment wrote {peak} bytes; limit {MAX_WRITABLE_BYTES}"
            )
    signal.alarm(0)
    print(
        json.dumps(
            {
                "packages": observed,
                "peak_writable_bytes": peak,
                "max_writable_bytes": MAX_WRITABLE_BYTES,
            },
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()
