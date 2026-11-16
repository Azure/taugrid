# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import ast
import os
from pathlib import Path
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

    def test_runtime_accepts_exact_downloaded_packages_in_virtualenv(self):
        self.modules["torch"] = self.torch
        with patch("runtime_contract.sys.prefix", "/tmp/ray/venv"):
            result = runtime_contract.check_runtime()
        self.assertEqual(result["torch"], "2.7.1+cu128")
        self.assertEqual(result["cuda"], "12.8")
        self.assertEqual(result["torch_file"], os.path.realpath(self.torch.__file__))
        self.assertEqual(result["prefix"], "/tmp/ray/venv")

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
