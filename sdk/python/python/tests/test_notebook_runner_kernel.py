# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""The shipped notebook runner must request the runtime image's kernel.

A notebook saved against a local kernelspec names it in ``metadata.kernelspec``.
nbclient's ``ExecutePreprocessor`` honours that name when no ``kernel_name`` is
given (``NotebookClient.create_kernel_manager``), so a pod running the notebook
asks the runtime image for a kernel it does not have and fails with
``NoSuchKernel`` before any cell executes. The runner must request the image's
own kernel instead.

This drives the real generated ``_tau_runner.py`` in a subprocess. nbconvert is
stubbed so the assertion is about the crate the runner ships, not about the
test environment's notebook stack: the stub mirrors nbclient's resolution order,
so any fix that makes the image's kernel the effective one passes.
"""

import json
import os
import subprocess
import sys
from pathlib import Path

from tau._notebook_pkg import RUNNER_NAME, package

#: The kernelspec a notebook would name after being saved in a local virtualenv.
LOCAL_KERNEL = "my-local-env"

#: The kernel the runtime image ships. ipykernel installs the ``python3``
#: kernelspec (its distribution data is share/jupyter/kernels/python3/kernel.json).
IMAGE_KERNEL = "python3"

_NBFORMAT_STUB = '''
import json
from pathlib import Path


def read(path, as_version=4):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def write(nb, path, version=4):
    Path(path).write_text(json.dumps(nb), encoding="utf-8")
'''

_NBCONVERT_STUB = '''
import os
from pathlib import Path


class ExecutePreprocessor:
    """Stands in for nbconvert and records the kernel the runner asks for.

    Mirrors nbclient's resolution order -- an explicit kernel_name wins, then
    the notebook's own kernelspec name, then the default -- so this asserts the
    effective kernel rather than one particular way of pinning it.
    """

    def __init__(self, **kwargs):
        self.kernel_name = kwargs.get("kernel_name")
        self.timeout = kwargs.get("timeout")

    def process_message(self, msg, cell, cell_index):
        return msg

    def preprocess(self, nb, resources=None, km=None):
        name = self.kernel_name
        if not name:
            name = (nb.get("metadata", {}).get("kernelspec", {}) or {}).get("name")
        if not name:
            name = "python3"
        Path(os.environ["TAU_KERNEL_PROBE"]).write_text(name, encoding="utf-8")
        return nb, (resources or {})
'''


def _notebook_naming(kernel: str) -> bytes:
    return json.dumps(
        {
            "nbformat": 4,
            "nbformat_minor": 5,
            "metadata": {
                "kernelspec": {"name": kernel, "display_name": kernel, "language": "python"},
                "language_info": {"name": "python"},
            },
            "cells": [
                {
                    "cell_type": "code",
                    "id": "c1",
                    "metadata": {},
                    "source": "print('hi')",
                    "outputs": [],
                    "execution_count": None,
                }
            ],
        }
    ).encode("utf-8")


def _stub_tree(root: Path) -> Path:
    root.mkdir(parents=True, exist_ok=True)
    (root / "nbformat.py").write_text(_NBFORMAT_STUB, encoding="utf-8")
    nbconvert = root / "nbconvert"
    (nbconvert / "preprocessors").mkdir(parents=True, exist_ok=True)
    (nbconvert / "__init__.py").write_text("from . import preprocessors\n", encoding="utf-8")
    (nbconvert / "preprocessors" / "__init__.py").write_text(_NBCONVERT_STUB, encoding="utf-8")
    return root


def test_runner_requests_the_image_kernel_not_a_local_kernelspec(tmp_path):
    staged = tmp_path / "stage"
    package(_notebook_naming(LOCAL_KERNEL), staging_dir=staged)

    probe = tmp_path / "requested-kernel.txt"
    stub = _stub_tree(tmp_path / "stub")
    env = dict(
        os.environ,
        PYTHONPATH=str(stub),
        TAU_KERNEL_PROBE=str(probe),
        TAU_DATA_DIR=str(tmp_path / "data"),
    )
    proc = subprocess.run(
        [sys.executable, str(staged / RUNNER_NAME), "--notebook", "analysis.ipynb"],
        cwd=str(staged),
        env=env,
        capture_output=True,
        text=True,
        timeout=120,
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    # The notebook names LOCAL_KERNEL; the runner must not ask the image for it.
    assert probe.read_text(encoding="utf-8") == IMAGE_KERNEL
