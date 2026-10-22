# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local tests for the validation-split exclusion in prepare_rl_prompts.

Needs no cobc and no GPU:
  python -m pytest examples/cobol-cpt-grpo/05-rl-prompts/test_prepare_rl_prompts.py -v
"""

import importlib.util
import json
import os

import pytest

RL_DIR = os.path.dirname(os.path.abspath(__file__))


@pytest.fixture
def prep(tmp_path, monkeypatch):
    monkeypatch.setenv("OUT", str(tmp_path / "prepared"))
    spec = importlib.util.spec_from_file_location(
        "prepare_rl_prompts", os.path.join(RL_DIR, "prepare_rl_prompts.py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _program(name, body_lines):
    head = [
        "       IDENTIFICATION DIVISION.",
        f"       PROGRAM-ID. {name}.",
        "       PROCEDURE DIVISION.",
    ]
    return "\n".join(head + body_lines) + "\n"


SHARED = [f"           DISPLAY 'SHARED LINE {i}'." for i in range(10)]
OTHER = [f"           DISPLAY 'TRAIN ONLY {i}'." for i in range(10)]


def _write_val(path, texts):
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w") as f:
        for text in texts:
            f.write(json.dumps({"text": text}) + "\n")


def test_val_paths_default_to_prepared_dir(prep, tmp_path):
    assert prep.VAL_PATH == str(tmp_path / "prepared" / "val.jsonl")
    assert prep.VAL_OVERLAP_LINES == 8


def test_windows_ignore_case_whitespace_and_blank_lines(prep):
    a = "\n".join(SHARED)
    b = "\n\n".join("  " + ln.lower().replace(" ", "   ") for ln in SHARED)
    assert prep.line_windows(a, 8) == prep.line_windows(b, 8)
    assert len(prep.line_windows(a, 8)) == 3


def test_exclude_val_overlap_drops_programs_sharing_a_window(prep, tmp_path):
    val_path = tmp_path / "prepared" / "val.jsonl"
    _write_val(val_path, [_program("VALPROG", SHARED)])
    windows, count = prep.load_val_windows(str(val_path), 8)
    assert count == 1

    copied = _program("RENAMED", OTHER[:2] + SHARED[1:9] + OTHER[2:4])
    short = _program("SHORT", OTHER[:3] + SHARED[:7] + OTHER[3:6])
    clean = _program("CLEAN", OTHER)
    kept, removed = prep.exclude_val_overlap(
        [("copied.cbl", copied), ("short.cbl", short), ("clean.cbl", clean)],
        windows,
        8,
    )
    assert removed == 1
    assert [p for p, _ in kept] == ["short.cbl", "clean.cbl"]


def test_exclude_val_overlap_checks_text_after_copy_stripping(prep, tmp_path):
    val_path = tmp_path / "prepared" / "val.jsonl"
    _write_val(val_path, [_program("VALPROG", SHARED)])
    windows, _ = prep.load_val_windows(str(val_path), 8)

    split = _program(
        "SPLIT", OTHER[:2] + SHARED[:4] + ["           COPY FOO."] + SHARED[4:8]
    )
    assert not prep.overlaps_val(split, windows, 8)
    assert prep.overlaps_val(prep.prepare_text(split), windows, 8)
    kept, removed = prep.exclude_val_overlap([("split.cbl", split)], windows, 8)
    assert removed == 1
    assert kept == []


def test_assert_disjoint_raises_on_overlap(prep, tmp_path):
    val_path = tmp_path / "prepared" / "val.jsonl"
    _write_val(val_path, [_program("VALPROG", SHARED)])
    windows, _ = prep.load_val_windows(str(val_path), 8)

    prep.assert_disjoint_from_val(
        [{"path": "clean.cbl", "text": _program("CLEAN", OTHER)}], windows, 8
    )
    with pytest.raises(RuntimeError, match="1 RL prompts overlap"):
        prep.assert_disjoint_from_val(
            [{"path": "val.cbl", "text": _program("VALPROG", SHARED)}], windows, 8
        )


@pytest.mark.parametrize("contents", [None, "", "\n\n"])
def test_load_val_windows_fails_without_validation_programs(prep, tmp_path, contents):
    val_path = tmp_path / "prepared" / "val.jsonl"
    if contents is not None:
        val_path.parent.mkdir(parents=True, exist_ok=True)
        val_path.write_text(contents)
    with pytest.raises(RuntimeError):
        prep.load_val_windows(str(val_path), 8)
