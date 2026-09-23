# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Tests for choosing which files ship with a submitted notebook."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from tau._notebook_pkg import package
from tau._payload import decode
from tau.jupyter import notebook_files
from tau.jupyter.notebook_files import (
    MAX_FILE_BYTES,
    FileSelectionError,
    list_candidates,
    notebook_directory,
    read_selected,
)

NOTEBOOK = "analysis.ipynb"


def notebook_bytes() -> bytes:
    return json.dumps({
        "nbformat": 4,
        "nbformat_minor": 5,
        "metadata": {"kernelspec": {"language": "python", "name": "python3"}, "language_info": {"name": "python"}},
        "cells": [{"cell_type": "code", "id": "c", "metadata": {}, "outputs": [], "execution_count": None, "source": ["print(1)"]}],
    }).encode()


def workspace(tmp_path: Path) -> Path:
    (tmp_path / NOTEBOOK).write_bytes(notebook_bytes())
    (tmp_path / "helpers.py").write_bytes(b"VALUE = 41\n")
    (tmp_path / "config.json").write_bytes(b"{}\n")
    (tmp_path / "notes.md").write_bytes(b"# notes\n")
    (tmp_path / "image.png").write_bytes(b"\x89PNG")
    (tmp_path / ".hidden").write_text("x", encoding="utf-8")
    (tmp_path / "__pycache__").mkdir()
    (tmp_path / "__pycache__" / "junk.pyc").write_bytes(b"x")
    (tmp_path / "nested").mkdir()
    (tmp_path / "nested" / "deep.py").write_text("x = 1\n", encoding="utf-8")
    return tmp_path


def test_candidates_offer_flat_source_files_and_skip_noise(tmp_path):
    root = workspace(tmp_path)

    result = list_candidates(root, NOTEBOOK)

    names = [row["name"] for row in result["files"]]
    assert names == ["config.json", "helpers.py", "notes.md"]
    # The notebook itself, hidden files, caches, binaries and nested trees are not offered.
    for skipped in (NOTEBOOK, ".hidden", "image.png", "nested/deep.py"):
        assert skipped not in names
    assert result["budgetBytes"] > 0


def test_candidates_mark_unreadable_directory_for_explicit_recovery(tmp_path):
    result = list_candidates(tmp_path, "missing/notebook.ipynb")
    assert result["files"] == []
    assert result["readable"] is False
    assert "permissions" in result["warnings"][0]


def test_candidates_report_directory_scan_bound(tmp_path, monkeypatch):
    monkeypatch.setattr(notebook_files, "MAX_CANDIDATES", 2)
    for name in ["one.py", "two.py", "three.py"]:
        (tmp_path / name).write_text("pass", encoding="utf-8")
    result = list_candidates(tmp_path, NOTEBOOK)
    assert len(result["files"]) == 2
    assert any("first 2 directory entries" in warning for warning in result["warnings"])


def test_candidates_warn_about_a_file_too_large_to_embed(tmp_path):
    root = workspace(tmp_path)
    (root / "big.py").write_bytes(b"x" * (MAX_FILE_BYTES + 1))

    result = list_candidates(root, NOTEBOOK)

    assert "big.py" not in [row["name"] for row in result["files"]]
    assert any("big.py" in warning for warning in result["warnings"])


def test_notebook_directory_rejects_escape_from_the_server_root(tmp_path):
    root = workspace(tmp_path)
    outside = tmp_path.parent / "elsewhere.ipynb"

    with pytest.raises(FileSelectionError):
        notebook_directory(root, "../" + outside.name)


def test_read_selected_refuses_paths_outside_the_flat_directory(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)

    for name in ("../helpers.py", "nested/deep.py", "/etc/passwd", ".."):
        with pytest.raises(FileSelectionError):
            read_selected(directory, [name])


def test_read_selected_refuses_missing_and_oversize_files(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    (root / "big.py").write_bytes(b"x" * (MAX_FILE_BYTES + 1))

    with pytest.raises(FileSelectionError):
        read_selected(directory, ["nope.py"])

    with pytest.raises(FileSelectionError) as failure:
        read_selected(directory, ["big.py"])
    assert failure.value.status == 413


def test_read_selected_enforces_the_payload_ceiling(tmp_path, monkeypatch):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    monkeypatch.setattr(notebook_files, "MAX_DECODED_BYTES", 10)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(directory, ["helpers.py", "config.json"])
    assert failure.value.status == 413
    assert "payload ceiling" in str(failure.value)


def test_chosen_files_ride_the_payload_and_the_plan_summary(tmp_path):
    root = workspace(tmp_path)
    selected = read_selected(notebook_directory(root, NOTEBOOK), ["helpers.py", "config.json"])

    staged = package(notebook_bytes(), staging_dir=tmp_path / "stage", extra_files=selected)

    assert staged.included_files == ["config.json", "helpers.py"]
    files = decode(staged.encoding.encoded, staged.encoding.digest)
    assert files["helpers.py"] == b"VALUE = 41\n"
    assert files["config.json"] == b"{}\n"
    # The chosen files are staged for inspection too.
    assert (tmp_path / "stage" / "helpers.py").exists()


def test_package_refuses_a_collision_with_a_generated_file(tmp_path):
    from tau._notebook_pkg import NotebookInvalid

    with pytest.raises(NotebookInvalid):
        package(notebook_bytes(), staging_dir=tmp_path, extra_files={"_tau_runner.py": b"x"})
