# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import sys
import textwrap
from pathlib import Path

from tau.datasets import dataset_file_reference
from tau.entrypoints import call_staged_function, load_staged_function, load_staged_module
from tau.jsonl import read_jsonl_objects


def _write_python(path: Path, source: str) -> None:
    path.write_text(textwrap.dedent(source).strip() + "\n")


def test_dataset_file_reference_normalizes_under_dataset_root(tmp_path):
    root = tmp_path / "dataset"
    image = root / "patient1" / "view1.png"
    image.parent.mkdir(parents=True)
    image.write_bytes(b"fake")

    rel_path, local_path = dataset_file_reference(root, "patient1\\study1\\..\\view1.png", field_name="image path")

    assert rel_path == "patient1/view1.png"
    assert local_path == image.resolve(strict=False)


def test_read_jsonl_objects_skips_blanks_and_caps_after_records(tmp_path):
    path = tmp_path / "records.jsonl"
    path.write_text('\n{"a": 1}\n\n{"a": 2}\n{"a": 3}\n', encoding="utf-8")

    assert read_jsonl_objects(path, max_records=2) == [{"a": 1}, {"a": 2}]


def test_load_staged_function_supports_sibling_imports_and_restores_sys_path(tmp_path):
    scripts = tmp_path / "scripts"
    scripts.mkdir()
    (scripts / "sibling.py").write_text('PREFIX = "ok"\n', encoding="utf-8")
    _write_python(
        scripts / "worker.py",
        """
        from sibling import PREFIX

        def make_value(value, *, suffix=""):
            return f"{PREFIX}:{value}{suffix}"
        """,
    )

    original_sys_path = list(sys.path)

    module = load_staged_module(scripts / "worker.py")
    make_value = load_staged_function(scripts / "worker.py", "make_value")

    assert module.make_value("x", suffix="!") == "ok:x!"
    assert make_value("y", suffix="?") == "ok:y?"
    assert sys.path == original_sys_path


def test_call_staged_function_forwards_args_and_kwargs(tmp_path):
    script = tmp_path / "task.py"
    _write_python(
        script,
        """
        def combine(left, right, *, sep=":"):
            return f"{left}{sep}{right}"
        """,
    )

    assert call_staged_function(script, "combine", "a", "b", sep="/") == "a/b"
