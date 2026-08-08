# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import sys
import textwrap
from pathlib import Path

import pytest

from tau import cli as tau_cli
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


@pytest.mark.parametrize("raw_path", ["", "   ", "."])
def test_dataset_file_reference_rejects_empty_or_root_reference(tmp_path, raw_path):
    with pytest.raises(ValueError, match="image path must reference a file under dataset root"):
        dataset_file_reference(tmp_path, raw_path, field_name="image path")


@pytest.mark.parametrize("raw_path", ["/tmp/outside.png", "C:\\data\\outside.png", "\\data\\outside.png"])
def test_dataset_file_reference_rejects_absolute_paths(tmp_path, raw_path):
    with pytest.raises(ValueError, match="image path must be relative to dataset root"):
        dataset_file_reference(tmp_path, raw_path, field_name="image path")


def test_dataset_file_reference_rejects_root_escape(tmp_path):
    with pytest.raises(ValueError, match="image path escapes dataset root"):
        dataset_file_reference(tmp_path, "../outside.png", field_name="image path")


def test_read_jsonl_objects_skips_blanks_and_caps_after_records(tmp_path):
    path = tmp_path / "records.jsonl"
    path.write_text('\n{"a": 1}\n\n{"a": 2}\n{"a": 3}\n', encoding="utf-8")

    assert read_jsonl_objects(path, max_records=2) == [{"a": 1}, {"a": 2}]


def test_read_jsonl_objects_rejects_empty_after_filtering(tmp_path):
    path = tmp_path / "empty.jsonl"
    path.write_text("\n \n", encoding="utf-8")

    with pytest.raises(ValueError, match="contains no records"):
        read_jsonl_objects(path)


def test_read_jsonl_objects_requires_objects_with_line_context(tmp_path):
    path = tmp_path / "records.jsonl"
    path.write_text('{"ok": true}\n[1, 2]\n', encoding="utf-8")

    with pytest.raises(ValueError, match=r"records\.jsonl:2 must contain a JSON object"):
        read_jsonl_objects(path)


def test_read_jsonl_objects_wraps_json_decode_errors(tmp_path):
    path = tmp_path / "bad.jsonl"
    path.write_text('{"ok": true}\n{"bad"\n', encoding="utf-8")

    with pytest.raises(ValueError, match=r"bad\.jsonl:2 invalid JSON"):
        read_jsonl_objects(path)


def test_read_jsonl_objects_rejects_negative_cap(tmp_path):
    with pytest.raises(ValueError, match="max_records must be non-negative"):
        read_jsonl_objects(tmp_path / "missing.jsonl", max_records=-1)


def test_load_staged_module_supports_sibling_imports_and_restores_sys_path(tmp_path):
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

    assert module.make_value("x", suffix="!") == "ok:x!"
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


def test_load_staged_function_rejects_non_callable(tmp_path):
    script = tmp_path / "task.py"
    script.write_text("VALUE = 1\n", encoding="utf-8")

    with pytest.raises(TypeError, match="is not callable"):
        load_staged_function(script, "VALUE")


def test_load_staged_module_removes_failed_import_from_sys_modules(tmp_path):
    script = tmp_path / "bad.py"
    script.write_text('raise RuntimeError("boom")\n', encoding="utf-8")

    with pytest.raises(RuntimeError, match="boom"):
        load_staged_module(script, module_name="bad_staged_module")

    assert "bad_staged_module" not in sys.modules


def test_load_staged_module_rejects_workloads_module_name(tmp_path):
    script = tmp_path / "worker.py"
    script.write_text("VALUE = 1\n", encoding="utf-8")

    with pytest.raises(ValueError, match="reserved by Tau"):
        load_staged_module(script, module_name="workloads")


def test_cli_module_loader_supports_sibling_imports(tmp_path):
    (tmp_path / "settings.py").write_text('NAME = "sibling-ok"\n', encoding="utf-8")
    _write_python(
        tmp_path / "experiment.py",
        """
        from settings import NAME

        def value():
            return NAME
        """,
    )

    original_sys_path = list(sys.path)

    module = tau_cli._load_module(tmp_path / "experiment.py")

    assert module.value() == "sibling-ok"
    assert sys.path == original_sys_path
