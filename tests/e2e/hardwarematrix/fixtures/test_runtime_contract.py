# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import ast
import hashlib
import os
from pathlib import Path
import re
import resource
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import runtime_contract


class RuntimeContractTest(unittest.TestCase):
    def setUp(self):
        self.ray = SimpleNamespace(__version__="2.58.0")
        self.torch = SimpleNamespace(
            __version__="2.7.1+cu128",
            version=SimpleNamespace(cuda="12.8"),
            __file__="/tmp/ray/runtime_resources/pip/env/site-packages/torch/__init__.py",
        )
        self.modules = {"ray": self.ray}
        for target, value in (
            ("sys.version_info", (3, 12, 16)),
            ("sys.prefix", "/usr"),
            ("sys.base_prefix", "/usr"),
            ("os.getuid", lambda: 65532),
            ("importlib", SimpleNamespace(import_module=self.import_module)),
        ):
            context = patch(f"runtime_contract.{target}", value)
            context.start()
            self.addCleanup(context.stop)
        context = patch.dict(os.environ, {
            "MATRIX_TORCH_SPEC": "torch==2.7.1",
            "MATRIX_TORCH_INDEX_URL": "https://download.pytorch.org/whl/cu128",
            "MATRIX_RAY_VERSION": "2.58.0",
        })
        context.start()
        self.addCleanup(context.stop)

    def import_module(self, name):
        if name not in self.modules:
            raise ModuleNotFoundError(name)
        return self.modules[name]

    def test_base_requires_ray_but_not_torch(self):
        self.assertEqual(runtime_contract.check_base("2.58.0")["uid"], 65532)
        self.modules.clear()
        with self.assertRaises(ModuleNotFoundError):
            runtime_contract.check_base("2.58.0")

    def test_base_rejects_virtualenv(self):
        with patch("runtime_contract.sys.prefix", "/tmp/ray/venv"):
            with self.assertRaisesRegex(RuntimeError, "global interpreter"):
                runtime_contract.check_base("2.58.0")

    def test_ib_rejects_low_inherited_memlock_without_changing_limits(self):
        for limits, passes in (
            ((65536, 65536), False),
            ((65536, 67108864), False),
            ((67108864, 67108864), True),
            ((resource.RLIM_INFINITY, resource.RLIM_INFINITY), True),
        ):
            with self.subTest(limits=limits):
                with patch("runtime_contract.resource.getrlimit", return_value=limits):
                    with patch("runtime_contract.resource.setrlimit") as change_limits:
                        if passes:
                            self.assertEqual(runtime_contract.check_memlock(), limits)
                        else:
                            with self.assertRaisesRegex(RuntimeError, "inherited soft/hard memlock"):
                                runtime_contract.check_memlock()
                        change_limits.assert_not_called()

    def test_ib_resolves_unindexed_cuda_before_set_device(self):
        source = ast.parse(Path(__file__).with_name("ib_gpu.py").read_text())
        loop = next(node for node in source.body if getattr(node, "name", "") == "collective_loop")
        collective = next(node for node in loop.body if isinstance(node, ast.Try))
        selected = []

        def set_device(device):
            if device.index is None:
                raise ValueError("Expected a torch.device with a specified index or an integer")
            selected.append(device.index)

        torch = SimpleNamespace(
            device=lambda kind, index=None: SimpleNamespace(type=kind, index=index),
            cuda=SimpleNamespace(current_device=lambda: 0, set_device=set_device),
        )
        with self.assertRaisesRegex(ValueError, "specified index"):
            torch.cuda.set_device(torch.device("cuda"))
        namespace = {"torch": torch}
        exec(compile(ast.Module(body=collective.body[:2], type_ignores=[]), "ib_gpu.py", "exec"), namespace)
        self.assertEqual(selected, [0])
        self.assertEqual(namespace["device"].type, "cuda")

    def test_ib_requires_real_transport_and_exact_unique_rank_results(self):
        source = ast.parse(Path(__file__).with_name("ib_gpu.py").read_text())
        functions = [node for node in source.body if isinstance(node, ast.FunctionDef)
                     and node.name in ("require_ib_transport", "require_rank_results")]
        namespace = {"re": re, "IB_HCAS": [f"mlx5_ib{index}" for index in range(8)]}
        exec(compile(ast.Module(body=functions, type_ignores=[]), "ib_gpu.py", "exec"), namespace)
        validate = namespace["require_rank_results"]
        log = "Bootstrap: Using eth0\nNET/IB : Using [0]mlx5_ib0:1/IB\nUsing network IB\n"
        records = [{"rank": rank, "hostname": f"worker-{rank}", "nccl_log": log,
                    "all_reduce": [float(index * 16 + 120) for index in range(16)]}
                   for rank in range(16)]
        validate(records, 16)
        for broken in (
            records[:-1],
            [dict(item, rank=0) for item in records],
            [dict(item, hostname="one-pod") for item in records],
            [dict(item, all_reduce=[0.0] * 16) for item in records],
            [dict(item, nccl_log="NET/IB : No device found\nUsing network Socket\n") for item in records],
            [dict(item, nccl_log=log + "NET/Socket : Using [0]eth0\n") for item in records],
            [dict(item, nccl_log="NET/IB : Using [0]mlx5_ib0:1/IB\n") for item in records],
            [dict(item, nccl_log=log.replace("mlx5_ib0:1/IB", "mlx5_an0:1/RoCE")) for item in records],
            [dict(item, nccl_log=log.replace("mlx5_ib0:1/IB", "mlx5_ib0:1/RoCE")) for item in records],
            [dict(item, nccl_log=log.replace("mlx5_ib0:1/IB", "mlx5_an0:1/IB")) for item in records],
        ):
            with self.subTest(broken=broken[0]):
                with self.assertRaises(RuntimeError):
                    validate(broken, 16)

    def test_ib_rejects_missing_inactive_or_ethernet_ports(self):
        source = ast.parse(Path(__file__).with_name("ib_gpu.py").read_text())
        function = next(node for node in source.body if getattr(node, "name", "") == "require_ib_devices")
        hcas = [f"mlx5_ib{index}" for index in range(8)]
        namespace = {"Path": Path, "os": os, "IB_HCAS": hcas}
        exec(compile(ast.Module(body=[function], type_ignores=[]), "ib_gpu.py", "exec"), namespace)
        with tempfile.TemporaryDirectory() as temporary, patch.dict(
            os.environ, {"NCCL_IB_HCA": "=" + ",".join(f"{hca}:1" for hca in hcas)},
        ):
            root = Path(temporary)
            for hca in hcas:
                port = root / hca / "ports" / "1"
                port.mkdir(parents=True)
                for name, value in (("link_layer", "InfiniBand"), ("state", "4: ACTIVE"),
                                    ("phys_state", "5: LinkUp")):
                    (port / name).write_text(value)
            validate = namespace["require_ib_devices"]
            self.assertEqual(len(validate(root)), 8)
            for name, invalid in (("link_layer", "Ethernet"), ("state", "1: DOWN"),
                                  ("phys_state", "2: Polling")):
                path = root / hcas[0] / "ports" / "1" / name
                original = path.read_text()
                path.write_text(invalid)
                with self.assertRaisesRegex(RuntimeError, "not ACTIVE/LinkUp"):
                    validate(root)
                path.write_text(original)
            path.unlink()
            with self.assertRaises(FileNotFoundError):
                validate(root)
            with patch.dict(os.environ, {"NCCL_IB_HCA": "mlx5"}):
                with self.assertRaisesRegex(RuntimeError, "exact approved"):
                    validate(root)

    def test_runtime_accepts_exact_downloaded_packages_in_virtualenv(self):
        self.modules["torch"] = self.torch
        with patch("runtime_contract.sys.prefix", "/tmp/ray/venv"):
            result = runtime_contract.check_runtime()
        self.assertEqual(result["torch"], "2.7.1+cu128")
        self.assertEqual(result["cuda"], "12.8")
        self.assertEqual(result["torch_file"], os.path.realpath(self.torch.__file__))
        self.assertEqual(result["prefix"], "/tmp/ray/venv")
        self.assertEqual(
            result["pip_spec_sha256"],
            hashlib.sha256(runtime_contract.PIP_SPEC_PATH.read_bytes()).hexdigest(),
        )

    def test_runtime_requires_mounted_lock_provenance(self):
        self.modules["torch"] = self.torch
        with tempfile.TemporaryDirectory() as temporary, patch(
            "runtime_contract.PIP_SPEC_PATH", Path(temporary) / "missing.json",
        ):
            with self.assertRaises(FileNotFoundError):
                runtime_contract.check_runtime()
            with self.assertRaises(FileNotFoundError):
                runtime_contract.runtime_pip_spec()

    def test_runtime_requires_torch_after_setup(self):
        with self.assertRaises(ModuleNotFoundError):
            runtime_contract.check_runtime()

    def test_wrong_runtime_packages_fail(self):
        self.modules["torch"] = self.torch
        for target, attribute, value in (
            (self.ray, "__version__", "2.57.0"),
            (self.torch, "__version__", "2.7.1+cpu"),
            (self.torch.version, "cuda", None),
            (self.torch.version, "cuda", "13.0"),
        ):
            with self.subTest(attribute=attribute, value=value):
                with patch.object(target, attribute, value):
                    with self.assertRaises(RuntimeError):
                        runtime_contract.check_runtime()

    def test_gpu_workers_reject_cpu_torch_before_gpu_operations(self):
        self.torch.__version__ = "2.7.1+cpu"
        self.torch.distributed = SimpleNamespace()
        self.torch.nn = SimpleNamespace()
        self.modules["torch"] = self.torch
        modules = {
            "torch": self.torch,
            "torch.distributed": self.torch.distributed,
            "torch.nn": self.torch.nn,
        }
        for fixture, owner, method in (
            ("serve_gpu.py", "GPUInference", "__init__"),
            ("train_gpu.py", None, "train_loop"),
            ("ib_gpu.py", None, "collective_loop"),
        ):
            with self.subTest(fixture=fixture):
                source = ast.parse(Path(__file__).with_name(fixture).read_text())
                nodes = source.body
                if owner:
                    nodes = next(node for node in nodes if getattr(node, "name", None) == owner).body
                function = next(node for node in nodes if getattr(node, "name", None) == method)
                namespace = {}
                exec(compile(ast.Module(body=[function], type_ignores=[]), fixture, "exec"), namespace)
                with patch.dict("sys.modules", modules):
                    with self.assertRaisesRegex(RuntimeError, "expected Torch 2.7.1\\+cu128"):
                        namespace[method](*([SimpleNamespace()] if owner else []))

    def test_wrong_interpreter_or_root_fails_both_stages(self):
        self.modules["torch"] = self.torch
        for target, value in (
            ("sys.version_info", (3, 13, 0)),
            ("os.getuid", lambda: 0),
        ):
            with self.subTest(target=target):
                with patch(f"runtime_contract.{target}", value):
                    with self.assertRaises(RuntimeError):
                        runtime_contract.check_base("2.58.0")
                    with self.assertRaises(RuntimeError):
                        runtime_contract.check_runtime()

    def test_unsupported_requested_packages_fail(self):
        self.modules["torch"] = self.torch
        for key, value in (
            ("MATRIX_TORCH_SPEC", "torch>=2.7.1"),
            ("MATRIX_TORCH_INDEX_URL", "https://pypi.org/simple"),
        ):
            with self.subTest(key=key):
                with patch.dict(os.environ, {key: value}):
                    with self.assertRaises(RuntimeError):
                        runtime_contract.check_runtime()


if __name__ == "__main__":
    unittest.main()
