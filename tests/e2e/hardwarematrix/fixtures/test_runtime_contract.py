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
            __version__="2.7.1+cu128", version=SimpleNamespace(cuda="12.8"),
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

    def test_base_without_torch_then_runtime_inside_virtualenv(self):
        runtime_contract.check_base("2.58.0")
        with self.assertRaisesRegex(RuntimeError, "unsupported Ray"):
            runtime_contract.check_base("2.57.0")
        with self.assertRaises(ModuleNotFoundError):
            runtime_contract.check_runtime()
        self.modules["torch"] = self.torch
        with patch("runtime_contract.sys.prefix", "/tmp/ray/venv"):
            runtime_contract.check_runtime()
            with self.assertRaisesRegex(RuntimeError, "global interpreter"):
                runtime_contract.check_base("2.58.0")

    def test_existing_package_and_identity_checks_are_not_skipped(self):
        self.modules["torch"] = self.torch
        for target, value in (
            ("sys.version_info", (3, 13, 0)), ("os.getuid", lambda: 0),
        ):
            with self.subTest(target=target), patch(f"runtime_contract.{target}", value):
                with self.assertRaises(RuntimeError):
                    runtime_contract.check_base("2.58.0")
                with self.assertRaises(RuntimeError):
                    runtime_contract.check_runtime()
        for target, attribute, value in (
            (self.ray, "__version__", "2.57.0"),
            (self.torch, "__version__", "2.7.1+cpu"),
            (self.torch.version, "cuda", "13.0"),
        ):
            with self.subTest(attribute=attribute), patch.object(target, attribute, value):
                with self.assertRaises(RuntimeError):
                    runtime_contract.check_runtime()
        for key, value in (
            ("MATRIX_TORCH_SPEC", "torch>=2.7.1"),
            ("MATRIX_TORCH_INDEX_URL", "https://pypi.org/simple"),
        ):
            with self.subTest(key=key), patch.dict(os.environ, {key: value}):
                with self.assertRaises(RuntimeError):
                    runtime_contract.check_runtime()

    def test_driver_and_worker_checks_run_before_gpu_operations(self):
        self.torch.__version__ = "2.7.1+cpu"
        self.torch.distributed = SimpleNamespace()
        self.torch.nn = SimpleNamespace()
        self.modules["torch"] = self.torch
        for fixture, owner, method in (
            ("serve_gpu.py", "GPUInference", "__init__"),
            ("train_gpu.py", None, "train_loop"),
        ):
            source = ast.parse(Path(__file__).with_name(fixture).read_text())
            self.assertTrue(any(
                isinstance(node, ast.Expr) and isinstance(node.value, ast.Call)
                and isinstance(node.value.func, ast.Name) and node.value.func.id == "check_runtime"
                for node in source.body
            ), fixture)
            nodes = source.body
            if owner:
                nodes = next(node for node in nodes if getattr(node, "name", "") == owner).body
            function = next(node for node in nodes if getattr(node, "name", "") == method)
            namespace = {}
            exec(compile(ast.Module(body=[function], type_ignores=[]), fixture, "exec"), namespace)
            with patch.dict("sys.modules", {
                "torch": self.torch, "torch.distributed": self.torch.distributed,
                "torch.nn": self.torch.nn,
            }):
                with self.assertRaisesRegex(RuntimeError, "expected Torch 2.7.1\\+cu128"):
                    namespace[method](*([SimpleNamespace()] if owner else []))


if __name__ == "__main__":
    unittest.main()
