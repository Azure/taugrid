# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import ast
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

import check_image

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent.parent
IMAGE = ROOT / "images/ray-torch"
PIP_SPEC = ROOT / "tests/e2e/hardwarematrix/fixtures/pip_spec.json"


class ImageContractTest(unittest.TestCase):
    def setUp(self):
        self.torch = SimpleNamespace(
            __version__="2.7.1+cu128",
            version=SimpleNamespace(cuda="12.8"),
            __file__="/usr/lib/python3.12/site-packages/torch/__init__.py",
        )
        self.ray = SimpleNamespace(__version__="2.58.0")
        for name, value in (
            ("sys.version_info", (3, 12, 16)),
            ("sys.prefix", "/usr"),
            ("sys.base_prefix", "/usr"),
            ("os.getuid", lambda: 65532),
            (
                "importlib",
                SimpleNamespace(
                    import_module=lambda name: {"torch": self.torch, "ray": self.ray}[name]
                ),
            ),
        ):
            target = patch(f"check_image.{name}", value)
            target.start()
            self.addCleanup(target.stop)

    def check(self, **overrides):
        arguments = {
            "torch_spec": "torch==2.7.1",
            "torch_index_url": "https://download.pytorch.org/whl/cu128",
            "ray_version": "2.58.0",
        }
        arguments.update(overrides)
        return check_image.check_image(**arguments)

    def test_global_exact_packages(self):
        self.assertEqual(self.check()["torch_file"], self.torch.__file__)
        self.assertEqual(self.check(torch_spec="torch==2.7.1+cu128")["uid"], 65532)

    def test_missing_package_fails(self):
        with patch(
            "check_image.importlib.import_module",
            side_effect=ModuleNotFoundError("torch"),
        ):
            with self.assertRaises(ModuleNotFoundError):
                self.check()

    def test_wrong_installed_packages_fail(self):
        for target, attribute, value in (
            (self.torch, "__version__", "2.7.1"),
            (self.torch.version, "cuda", "12.6"),
            (self.ray, "__version__", "2.57.0"),
        ):
            with self.subTest(attribute=attribute, value=value):
                with patch.object(target, attribute, value):
                    with self.assertRaises(RuntimeError):
                        self.check()

    def test_incompatible_requests_fail_before_import(self):
        for overrides in (
            {"torch_spec": "torch>=2.7.1"},
            {"torch_index_url": "https://pypi.org/simple"},
            {"ray_version": "2.57.0"},
        ):
            with self.subTest(overrides=overrides):
                with patch("check_image.importlib.import_module") as imported:
                    with self.assertRaises(RuntimeError):
                        self.check(**overrides)
                    imported.assert_not_called()

    def test_wrong_interpreter_or_root_fails(self):
        for target, value in (
            ("sys.prefix", "/tmp/runtime-env"),
            ("sys.version_info", (3, 13, 0)),
            ("os.getuid", lambda: 0),
        ):
            with self.subTest(target=target):
                with patch(f"check_image.{target}", value):
                    with self.assertRaises(RuntimeError):
                        self.check()


class SourceContractTest(unittest.TestCase):
    def test_locked_install_refuses_modified_wheels_and_missing_transitives(self):
        runtime = json.loads(PIP_SPEC.read_text())
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)

            def wheel(name, version, requires=()):
                stem = f"{name.replace('-', '_')}-{version}"
                path = root / f"{stem}-py3-none-any.whl"
                with zipfile.ZipFile(path, "w") as archive:
                    archive.writestr(
                        f"{stem}.dist-info/METADATA",
                        f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n"
                        + "".join(f"Requires-Dist: {item}\n" for item in requires),
                    )
                    archive.writestr(
                        f"{stem}.dist-info/WHEEL",
                        "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
                    )
                    archive.writestr(f"{stem}.dist-info/RECORD", "")
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
                return path, f"{name}=={version} --hash=sha256:{digest}"

            parent, parent_requirement = wheel(
                "taugrid-lock-parent", "1.0", ["taugrid-lock-child==1.0"],
            )
            _, child_requirement = wheel("taugrid-lock-child", "1.0")

            def install(packages):
                requirements = root / "requirements.txt"
                requirements.write_text("\n".join(packages) + "\n")
                return subprocess.run(
                    [
                        sys.executable, "-m", "pip", "install", "--dry-run",
                        "--ignore-installed", "--target", str(root / "installed"),
                        "-r", str(requirements),
                        *runtime["pip_install_options"],
                        "--no-index", "--find-links", str(root),
                    ],
                    capture_output=True, text=True, timeout=30,
                )

            result = install([parent_requirement, child_requirement])
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            result = install([parent_requirement])
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Hashes are required", result.stderr)

            with parent.open("ab") as output:
                output.write(b"modified")
            result = install([parent_requirement, child_requirement])
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("DO NOT MATCH THE HASHES", result.stderr)

            wheel("torch", "2.7.1+cu128")
            result = install(runtime["packages"])
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("DO NOT MATCH THE HASHES", result.stderr)

    def test_image_and_runtime_use_one_primary_index(self):
        source = (IMAGE / "Dockerfile").read_text().replace("\\\n", " ")
        install = next(
            line for line in source.splitlines()
            if line.startswith("RUN python3 -m pip install ")
        )
        parser = argparse.ArgumentParser()
        parser.add_argument("--no-cache-dir", action="store_true", required=True)
        parser.add_argument("--isolated", action="store_true")
        parser.add_argument("--disable-pip-version-check", action="store_true")
        parser.add_argument("--require-hashes", action="store_true")
        parser.add_argument("--only-binary")
        parser.add_argument("--index-url", required=True)
        parser.add_argument("--requirement")
        parser.add_argument("packages", nargs="*")
        image_options = parser.parse_args(shlex.split(install.split("&&")[0])[5:])
        runtime = json.loads(PIP_SPEC.read_text())
        runtime_options = parser.parse_args(runtime["pip_install_options"] + runtime["packages"])
        self.assertEqual(image_options.index_url, "https://download.pytorch.org/whl/cu128")
        self.assertEqual(runtime_options.index_url, image_options.index_url)
        self.assertEqual(image_options.packages, [])
        self.assertEqual(image_options.requirement, "/tmp/torch-requirements.txt")
        for options in (runtime_options, image_options):
            self.assertTrue(options.require_hashes)
            self.assertTrue(options.isolated)
            self.assertEqual(options.only_binary, ":all:")
        for requirement in runtime_options.packages:
            self.assertRegex(requirement, r"^[a-z0-9-]+==[^ ]+")
            self.assertRegex(requirement, r" --hash=sha256:[0-9a-f]{64}( |$)")

    def test_serve_pip_spec_matches_image_smoke(self):
        source = ast.parse(
            (ROOT / "tests/e2e/hardwarematrix/fixtures/serve_gpu.py").read_text()
        )
        deployment = next(
            node
            for node in source.body
            if isinstance(node, ast.ClassDef) and node.name == "GPUInference"
        )
        options = next(
            keyword.value
            for keyword in deployment.decorator_list[0].keywords
            if keyword.arg == "ray_actor_options"
        )
        environment = next(value for key, value in zip(
            options.keys, options.values,
        ) if ast.literal_eval(key) == "runtime_env")
        pip = next(value for key, value in zip(
            environment.keys, environment.values,
        ) if ast.literal_eval(key) == "pip")
        self.assertIsInstance(pip, ast.Call)
        self.assertEqual(pip.func.id, "runtime_pip_spec")
        self.assertEqual(pip.args, [])
        self.assertEqual(pip.keywords, [])
        module_spec = importlib.util.spec_from_file_location(
            "runtime_contract", PIP_SPEC.with_name("runtime_contract.py"),
        )
        module = importlib.util.module_from_spec(module_spec)
        module_spec.loader.exec_module(module)
        self.assertEqual(module.runtime_pip_spec(), json.loads(PIP_SPEC.read_text()))
        self.assertIn(
            "COPY tests/e2e/hardwarematrix/fixtures/pip_spec.json "
            "/opt/taugrid/ray-torch/pip_spec.json",
            (IMAGE / "Dockerfile").read_text(),
        )

    def test_make_interface_and_base_tests_are_reused(self):
        for arch in ("amd64", "arm64"):
            result = subprocess.run(
                [
                    "make", "-n", "docker-push-arch", f"ARCH={arch}",
                    "IMG=registry.invalid/ray:fixed-torch2.7.1-cu128",
                    "CACHE_REPO=registry.invalid/ray-torch-cache",
                    "SOURCE_REVISION=fixture",
                ],
                cwd=IMAGE, capture_output=True, text=True, check=True,
            )
            self.assertIn(f"--platform linux/{arch}", result.stdout)
            self.assertIn(f"-torch2.7.1-cu128-{arch}", result.stdout)
            self.assertIn(f"ray-torch-cache:cache-{arch}", result.stdout)
            self.assertIn("--file Dockerfile", result.stdout)
            self.assertIn("--push ../..", result.stdout)
        result = subprocess.run(
            ["make", "-n", "test", "IMG=registry.invalid/ray:fixed"],
            cwd=IMAGE, capture_output=True, text=True, check=True,
        )
        self.assertIn("test_image.sh", result.stdout)
        self.assertIn("python3 -m pip check", result.stdout)

    def test_make_rejects_unsupported_tuple_before_push(self):
        for override in (
            "PYTHON_VERSION=3.13", "RAY_VERSION=2.57.0", "CUDA_VERSION=12.8",
        ):
            result = subprocess.run(
                ["make", "-n", "docker-push-arch", "ARCH=amd64", override],
                cwd=IMAGE, capture_output=True, text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("ray-torch requires", result.stderr)
            self.assertNotIn("docker buildx build", result.stdout)

    def test_writable_layer_gate_and_container_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory)
            docker = binary / "docker"
            calls = binary / "calls"
            docker.write_text(
                "#!/usr/bin/env bash\n"
                'printf "%s\\n" "$*" >>"$DOCKER_CALLS"\n'
                'case "$1" in\n'
                'create) echo fixture ;;\n'
                'inspect) if [[ "$*" == *ExitCode* ]]; then echo "$IMAGE_STATUS"; '
                'else echo "$WRITABLE_BYTES"; fi ;;\n'
                'esac\n'
            )
            docker.chmod(0o755)
            for status, size, expected in (
                ("0", "536870912", 0),
                ("0", "536870913", 1),
                ("0", "<nil>", 1),
                ("7", "1", 1),
            ):
                with self.subTest(status=status, size=size):
                    calls.write_text("")
                    environment = dict(os.environ)
                    environment.update(
                        PATH=f"{binary}:{environment['PATH']}",
                        DOCKER_CALLS=str(calls),
                        IMAGE_STATUS=status,
                        WRITABLE_BYTES=size,
                    )
                    result = subprocess.run(
                        ["bash", str(HERE / "test_image.sh"), "fixture-image"],
                        env=environment, capture_output=True, text=True,
                    )
                    self.assertEqual(result.returncode, expected, result.stderr)
                    self.assertEqual(calls.read_text().splitlines()[-1], "rm -f fixture")


if __name__ == "__main__":
    unittest.main()
