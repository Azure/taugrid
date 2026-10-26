# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Selected companion files must reach Ray tasks, not only the driver.

Workers mount their own empty ``/data`` and ``runtimeEnvYAML`` carried pip
packages only, so a selected helper that reached the head was missing for every
remote task, even though preview listed the file as included.

The runner publishes the selected files as the ``runtime_env`` working_dir
archive that every Ray node resolves, and loads the selected modules from that
directory before any notebook code runs. These tests drive the generated runner
and then check what a worker resolves from that archive.
"""

from __future__ import annotations

import io
import json
import os
import subprocess
import sys
import zipfile
from pathlib import Path

import pytest
import yaml
from tau._notebook_pkg import DISTRIBUTED_ARCHIVE_NAME, RUNNER_NAME, NotebookInvalid, package
from tau._payload import TARGET_DIR
from tau._render import ENV_TARGET_DIR, GPU_EXECUTION_RAY, Profile, _runtime_env_yaml, render_rayjob

NBFORMAT_STUB = '''
import json
from pathlib import Path


def read(path, as_version=4):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def write(nb, path, version=4):
    Path(path).write_text(json.dumps(nb), encoding="utf-8")
'''

NBCONVERT_STUB = '''
import os
import sys
from pathlib import Path


class ExecutePreprocessor:
    def __init__(self, **kwargs):
        self.kernel_name = kwargs.get("kernel_name")
        self.timeout = kwargs.get("timeout")

    def process_message(self, msg, cell, cell_index):
        return msg

    def preprocess(self, nb, resources=None, km=None):
        work = Path((resources or {}).get("metadata", {}).get("path", "."))
        scope = {"__name__": "__main__"}
        cwd = Path.cwd()
        os.chdir(work)
        sys.path.insert(0, str(work))
        try:
            for cell in nb.get("cells", []):
                exec(compile("".join(cell.get("source") or []), "<cell>", "exec"), scope)
        finally:
            sys.path.remove(str(work))
            os.chdir(cwd)
        return nb, (resources or {})
'''


def _notebook(*sources: str) -> bytes:
    cells = [
        {"cell_type": "code", "id": f"c{index}", "metadata": {}, "outputs": [],
         "execution_count": None, "source": [source]}
        for index, source in enumerate(sources)
    ]
    return json.dumps(
        {
            "nbformat": 4,
            "nbformat_minor": 5,
            "metadata": {
                "kernelspec": {"language": "python", "name": "python3"},
                "language_info": {"name": "python"},
            },
            "cells": cells,
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


def _run_runner(runner: Path, env: dict) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(runner), "--notebook", "analysis.ipynb"],
        cwd=str(runner.parent),
        env=env,
        capture_output=True,
        text=True,
        timeout=120,
    )


def _env(tmp_path: Path, script: Path) -> dict:
    return dict(
        os.environ,
        PYTHONPATH=str(_stub_tree(tmp_path / "stub")),
        TAU_DATA_DIR=str(tmp_path / "data"),
        TAU_PAYLOAD_TARGET_DIR=str(script),
    )


def test_the_archive_ships_in_the_payload_before_the_driver_runs(tmp_path):
    selected = {
        "helpers.py": b"GREETING = 'hello from the selected helper'\n",
        "config.json": b'{"answer": 42}'+b"\n",
    }
    staged = package(
        _notebook("print('hi')"),
        staging_dir=tmp_path / "stage",
        extra_files=selected,
        distributed=True,
    )
    rendered = render_rayjob(
        name="companions",
        namespace="ray",
        queue="q",
        profile=Profile(name="cpu", workers=1),
        entrypoint="python3 /script/_tau_runner.py --notebook analysis.ipynb",
        payload=staged.encoding,
        gpu_execution=GPU_EXECUTION_RAY,
    )
    worker = rendered["spec"]["rayClusterSpec"]["workerGroupSpecs"][0]["template"]["spec"]
    assert worker["initContainers"][0]["name"] == "tau-payload"

    archive_bytes = staged.files[DISTRIBUTED_ARCHIVE_NAME]
    worker_dir = tmp_path / "worker"
    worker_dir.mkdir()
    with zipfile.ZipFile(io.BytesIO(archive_bytes)) as bundle:
        bundle.extractall(worker_dir)

    assert (worker_dir / "helpers.py").read_bytes() == selected["helpers.py"]
    assert (worker_dir / "config.json").read_bytes() == selected["config.json"]
    names = {path.name for path in worker_dir.iterdir()}
    assert RUNNER_NAME not in names
    assert "analysis.ipynb" not in names


def test_the_driver_no_longer_writes_the_archive(tmp_path):
    staged = package(
        _notebook("print('hi')"),
        staging_dir=tmp_path / "stage",
        extra_files={"helpers.py": b"VALUE = 7\n"},
        distributed=True,
    )
    result = _run_runner(staged.runner_path, _env(tmp_path, tmp_path / "stage"))
    assert result.returncode == 0, result.stdout + result.stderr
    assert not (tmp_path / "stage" / DISTRIBUTED_ARCHIVE_NAME).exists()


def test_a_local_submission_ships_no_archive(tmp_path):
    staged = package(_notebook("print('hi')"), extra_files={"helpers.py": b"VALUE = 7\n"})
    assert DISTRIBUTED_ARCHIVE_NAME not in staged.files


def test_the_runner_still_resolves_a_selected_helper(tmp_path):
    staged = package(
        _notebook("import helpers", "print(helpers.VALUE)"),
        staging_dir=tmp_path / "stage",
        extra_files={"helpers.py": b"VALUE = 7\n"},
        distributed=True,
    )
    result = _run_runner(staged.runner_path, _env(tmp_path, tmp_path / "stage"))
    assert result.returncode == 0, result.stdout + result.stderr
    with zipfile.ZipFile(io.BytesIO(staged.files[DISTRIBUTED_ARCHIVE_NAME])) as bundle:
        assert "helpers.py" in bundle.namelist()



def test_distributed_runtime_env_uses_string_env_var_values():
    rendered = yaml.safe_load(_runtime_env_yaml([], distributed=True))
    env_vars = rendered["env_vars"]
    assert env_vars[ENV_TARGET_DIR] == TARGET_DIR
    assert all(isinstance(value, str) for value in env_vars.values())





def test_the_runner_does_not_execute_companion_modules():
    from tau._notebook_pkg import _runner_script

    script = _runner_script(600)
    assert "exec_module" not in script
    assert "spec_from_file_location" not in script


def test_a_companion_is_imported_by_a_cell_not_before_the_cells(tmp_path):
    staged = package(
        _notebook(
            "import pathlib; pathlib.Path('cell-ran').write_text('1')",
            "import helper",
            "print(helper.VALUE)",
        ),
        staging_dir=tmp_path / "stage",
        extra_files={"helper.py": b"import pathlib\nVALUE = pathlib.Path('cell-ran').exists()\n"},
    )
    result = _run_runner(staged.runner_path, _env(tmp_path, tmp_path / "stage"))
    assert result.returncode == 0, result.stdout + result.stderr
    assert "True" in result.stdout


def test_a_companion_whose_body_would_fail_at_import_time_does_not_stop_the_run(tmp_path):
    staged = package(
        _notebook("print('cells still run')"),
        staging_dir=tmp_path / "stage",
        extra_files={"helper.py": b"raise RuntimeError('must not run before cells')\n"},
    )
    result = _run_runner(staged.runner_path, _env(tmp_path, tmp_path / "stage"))
    assert result.returncode == 0, result.stdout + result.stderr
    assert "cells still run" in result.stdout


def test_the_companion_is_transported_to_the_working_directory(tmp_path):
    staged = package(
        _notebook("print('hi')"),
        staging_dir=tmp_path / "stage",
        extra_files={"helper.py": b"VALUE = 1\n"},
    )
    result = _run_runner(staged.runner_path, _env(tmp_path, tmp_path / "stage"))
    assert result.returncode == 0, result.stdout + result.stderr
    assert (tmp_path / "stage" / "helper.py").read_bytes() == b"VALUE = 1\n"
    assert "staged 1 file(s)" in result.stdout

def test_a_selected_file_named_like_the_archive_is_refused_not_overwritten():
    with pytest.raises(NotebookInvalid):
        package(
            _notebook("print('hi')"),
            extra_files={DISTRIBUTED_ARCHIVE_NAME: b"USER-CONTROLLED"},
            distributed=True,
        )


def test_the_archive_name_is_allowed_when_not_distributed():
    staged = package(
        _notebook("print('hi')"),
        extra_files={DISTRIBUTED_ARCHIVE_NAME: b"USER-CONTROLLED"},
    )
    assert staged.files[DISTRIBUTED_ARCHIVE_NAME] == b"USER-CONTROLLED"