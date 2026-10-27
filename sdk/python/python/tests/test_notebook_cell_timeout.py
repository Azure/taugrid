# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""The executed-notebook cell timeout must be configurable.

The runner constructed ``StreamingExecutor(timeout=600)`` with a hardcoded ten
minutes and neither submission path exposed an override, so a legitimate long
training or ``ray.get`` cell was killed and the executed notebook was never
written. The timeout stays bounded by default; callers may configure it.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from tau._notebook_pkg import (
    DEFAULT_CELL_TIMEOUT_SECONDS,
    MAX_CELL_TIMEOUT_SECONDS,
    NotebookInvalid,
    RUNNER_NAME,
    package,
)

NBFORMAT_STUB = '''
import json
from pathlib import Path


def read(path, as_version=4):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def write(nb, path, version=4):
    Path(path).write_text(json.dumps(nb), encoding="utf-8")
'''

NBCONVERT_STUB = '''
import json
import os
import time
from pathlib import Path


class ExecutePreprocessor:
    def __init__(self, **kwargs):
        self.kernel_name = kwargs.get("kernel_name")
        self.timeout = kwargs.get("timeout")

    def process_message(self, msg, cell, cell_index):
        return msg

    def preprocess(self, nb, resources=None, km=None):
        Path(os.environ["TIMEOUT_PROBE"]).write_text(json.dumps(self.timeout), encoding="utf-8")
        cell = nb["cells"][0]
        budget = float(os.environ.get("SLOW_CELL_SECONDS", "0.2"))
        started = time.monotonic()
        while time.monotonic() - started < budget:
            time.sleep(0.01)
        cell["outputs"] = [{"output_type": "stream", "name": "stdout", "text": "done\\n"}]
        return nb, (resources or {})
'''


def _notebook() -> bytes:
    return json.dumps(
        {
            "nbformat": 4,
            "nbformat_minor": 5,
            "metadata": {
                "kernelspec": {"language": "python", "name": "python3"},
                "language_info": {"name": "python"},
            },
            "cells": [
                {"cell_type": "code", "id": "c0", "metadata": {}, "outputs": [],
                 "execution_count": None, "source": ["print('train')"]}
            ],
        }
    ).encode("utf-8")


def _stub_tree(root: Path) -> Path:
    root.mkdir(parents=True, exist_ok=True)
    (root / "nbformat.py").write_text(NBFORMAT_STUB, encoding="utf-8")
    nbconvert = root / "nbconvert"
    (nbconvert / "preprocessors").mkdir(parents=True, exist_ok=True)
    (nbconvert / "__init__.py").write_text("from . import preprocessors\n", encoding="utf-8")
    (nbconvert / "preprocessors" / "__init__.py").write_text(NBCONVERT_STUB, encoding="utf-8")
    return root


def _run(tmp_path: Path, staged, env_extra: dict | None = None) -> subprocess.CompletedProcess:
    env = dict(
        os.environ,
        PYTHONPATH=str(_stub_tree(tmp_path / "stub")),
        TAU_DATA_DIR=str(tmp_path / "data"),
        TIMEOUT_PROBE=str(tmp_path / "timeout.json"),
    )
    env.update(env_extra or {})
    return subprocess.run(
        [sys.executable, str(staged.runner_path), "--notebook", "analysis.ipynb"],
        cwd=str(staged.runner_path.parent),
        env=env,
        capture_output=True,
        text=True,
        timeout=120,
    )


def _configured_timeout(tmp_path: Path) -> object:
    return json.loads((tmp_path / "timeout.json").read_text(encoding="utf-8"))


def test_the_runner_honours_a_configured_cell_timeout(tmp_path):
    staged = package(_notebook(), staging_dir=tmp_path / "stage", cell_timeout_seconds=7200)
    result = _run(tmp_path, staged)
    assert result.returncode == 0, result.stdout + result.stderr
    assert _configured_timeout(tmp_path) == 7200


def test_the_default_cell_timeout_remains_bounded(tmp_path):
    staged = package(_notebook(), staging_dir=tmp_path / "stage")
    result = _run(tmp_path, staged)
    assert result.returncode == 0, result.stdout + result.stderr
    configured = _configured_timeout(tmp_path)
    assert configured == DEFAULT_CELL_TIMEOUT_SECONDS
    assert 0 < configured <= MAX_CELL_TIMEOUT_SECONDS


def test_a_slow_progressing_cell_past_the_old_bound_still_writes_the_notebook(tmp_path):
    probe_seconds = 3
    staged = package(_notebook(), staging_dir=tmp_path / "stage", cell_timeout_seconds=probe_seconds * 4)
    result = _run(tmp_path, staged, {"SLOW_CELL_SECONDS": str(probe_seconds)})
    assert result.returncode == 0, result.stdout + result.stderr
    assert _configured_timeout(tmp_path) > probe_seconds

    executed = tmp_path / "data" / "analysis.executed.ipynb"
    assert executed.is_file()
    outputs = json.loads(executed.read_text(encoding="utf-8"))["cells"][0]["outputs"]
    assert outputs and outputs[0]["text"] == "done\n"


@pytest.mark.parametrize("value", [0, -1, True, 1.5, "600", None])
def test_an_unbounded_or_nonpositive_timeout_is_refused(tmp_path, value):
    with pytest.raises(NotebookInvalid):
        package(_notebook(), staging_dir=tmp_path / "stage", cell_timeout_seconds=value)


def test_the_timeout_is_threaded_from_the_submission_caller(tmp_path):
    from tests.test_jupyter_submit import VALID_NB
    from tau._profile import Profile
    from tau.jupyter import submit

    plan = submit.build_plan(
        client=None, notebook_bytes=VALID_NB, profile=Profile(name="cpu", workers=1),
        namespace="ray", staging_dir=tmp_path / "stage", cell_timeout_seconds=3600,
    )
    runner = plan.staged.encoding.files[RUNNER_NAME].decode("utf-8")
    assert "CELL_TIMEOUT_SECONDS = 3600" in runner
    assert "StreamingExecutor(timeout=CELL_TIMEOUT_SECONDS" in runner
