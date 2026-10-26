# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Tests for choosing which files ship with a submitted notebook."""

from __future__ import annotations

import json
import os
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
    root = tmp_path / "notebooks"
    root.mkdir()
    (root / NOTEBOOK).write_bytes(notebook_bytes())
    (root / "helpers.py").write_bytes(b"VALUE = 41\n")
    (root / "config.json").write_bytes(b"{}\n")
    (root / "notes.md").write_bytes(b"# notes\n")
    (root / "image.png").write_bytes(b"\x89PNG")
    (root / ".hidden").write_text("x", encoding="utf-8")
    (root / "__pycache__").mkdir()
    (root / "__pycache__" / "junk.pyc").write_bytes(b"x")
    (root / "nested").mkdir()
    (root / "nested" / "deep.py").write_text("x = 1\n", encoding="utf-8")
    return root


def sibling_directory(tmp_path: Path, suffix: str) -> Path:
    return tmp_path / f"{tmp_path.name}-{suffix}"


def authorized_directory_replaced_by_a_symlink(tmp_path: Path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    authorized = sibling_directory(tmp_path, "authorized")
    attacker = sibling_directory(tmp_path, "attacker")
    directory.rename(authorized)
    attacker.mkdir()
    (attacker / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    directory.symlink_to(attacker, target_is_directory=True)
    return root, directory, attacker


def test_candidates_offer_flat_source_files_and_skip_noise(tmp_path):
    root = workspace(tmp_path)

    result = list_candidates(notebook_directory(root, NOTEBOOK), NOTEBOOK)

    names = [row["name"] for row in result["files"]]
    assert names == ["config.json", "helpers.py", "notes.md"]
    # The notebook itself, hidden files, caches, binaries and nested trees are not offered.
    for skipped in (NOTEBOOK, ".hidden", "image.png", "nested/deep.py"):
        assert skipped not in names
    assert result["budgetBytes"] > 0


def test_candidates_mark_unreadable_directory_for_explicit_recovery(tmp_path):
    result = list_candidates(notebook_directory(tmp_path, "missing/notebook.ipynb"), "missing/notebook.ipynb")
    assert result["files"] == []
    assert result["readable"] is False
    assert "permissions" in result["warnings"][0]


def test_candidates_report_directory_scan_bound(tmp_path, monkeypatch):
    monkeypatch.setattr(notebook_files, "MAX_CANDIDATES", 2)
    for name in ["one.py", "two.py", "three.py"]:
        (tmp_path / name).write_text("pass", encoding="utf-8")
    result = list_candidates(notebook_directory(tmp_path, NOTEBOOK), NOTEBOOK)
    assert len(result["files"]) == 2
    assert any("first 2 directory entries" in warning for warning in result["warnings"])


def test_candidates_warn_about_a_file_too_large_to_embed(tmp_path):
    root = workspace(tmp_path)
    (root / "big.py").write_bytes(b"x" * (MAX_FILE_BYTES + 1))

    result = list_candidates(notebook_directory(root, NOTEBOOK), NOTEBOOK)

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
            read_selected(root, directory, [name])


def test_read_selected_refuses_files_the_picker_skips(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)

    for name in (".hidden", "image.png"):
        with pytest.raises(FileSelectionError):
            read_selected(root, directory, [name])


def test_read_selected_refuses_missing_and_oversize_files(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    (root / "big.py").write_bytes(b"x" * (MAX_FILE_BYTES + 1))

    with pytest.raises(FileSelectionError):
        read_selected(root, directory, ["nope.py"])

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["big.py"])
    assert failure.value.status == 413


def test_read_selected_bounds_the_read_itself(tmp_path, monkeypatch):
    """An oversized file must be refused without pulling it into memory.

    The preview endpoint accepts names independently of the picker, so a
    multi-GB file (or one that grew after selection) must not be allocated
    before the ceiling is enforced.
    """
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    (root / "huge.py").write_bytes(b"x" * (MAX_FILE_BYTES * 8))

    real_fdopen = os.fdopen
    reads = []

    class CountingHandle:
        def __init__(self, handle):
            self._handle = handle

        def __enter__(self):
            self._handle.__enter__()
            return self

        def __exit__(self, *exc):
            return self._handle.__exit__(*exc)

        def read(self, size=-1):
            reads.append(size)
            return self._handle.read(size)

    def counting_fdopen(fd, *args, **kwargs):
        return CountingHandle(real_fdopen(fd, *args, **kwargs))

    monkeypatch.setattr(os, "fdopen", counting_fdopen)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["huge.py"])

    assert failure.value.status == 413
    # The read asked for a bounded slice and stopped at the ceiling rather
    # than consuming the whole file.
    assert reads == [MAX_FILE_BYTES + 1]


def test_read_selected_enforces_the_payload_ceiling(tmp_path, monkeypatch):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    monkeypatch.setattr(notebook_files, "MAX_DECODED_BYTES", 10)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["helpers.py", "config.json"])
    assert failure.value.status == 413
    assert "payload ceiling" in str(failure.value)


def test_read_selected_refuses_a_directory_swapped_for_a_symlink(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    assert read_selected(root, directory, ["helpers.py"], before_read=lambda: None) == {"helpers.py": b"VALUE = 41\n"}

    authorized = sibling_directory(tmp_path, "authorized")
    directory.rename(authorized)
    attacker = sibling_directory(tmp_path, "attacker")
    attacker.mkdir()
    (attacker / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    directory.symlink_to(attacker, target_is_directory=True)

    with pytest.raises(FileSelectionError):
        read_selected(root, directory, ["helpers.py"])
    assert (attacker / "helpers.py").read_bytes() == b"ATTACKER = 1\n"


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="the bound branch requires POSIX dir_fd and O_NOFOLLOW")
def test_read_selected_refuses_a_file_swapped_for_a_symlink_between_list_and_read(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    assert [row["name"] for row in list_candidates(directory, NOTEBOOK)["files"]] == ["config.json", "helpers.py", "notes.md"]

    secret = tmp_path / "outside_secret.txt"
    secret.write_bytes(b"SA-TOKEN-abcdef")
    target = root / "helpers.py"
    target.unlink()
    target.symlink_to(secret)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["helpers.py"])
    assert failure.value.status == 409


def test_read_selected_refuses_a_file_replaced_after_selection(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    secret = tmp_path / "outside_secret.txt"
    secret.write_bytes(b"SA-TOKEN-abcdef")

    def swap():
        target = root / "helpers.py"
        target.unlink()
        target.symlink_to(secret)

    with pytest.raises(FileSelectionError):
        read_selected(root, directory, ["helpers.py"], before_read=swap)


def test_read_selected_refuses_an_alias_created_after_selection(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    secret = tmp_path / "outside_secret.txt"
    secret.write_bytes(b"SA-TOKEN-abcdef")

    def create_alias():
        (root / "late.py").symlink_to(secret)

    with pytest.raises(FileSelectionError):
        read_selected(root, directory, ["late.py"], before_read=create_alias)


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_read_selected_binds_to_the_opened_directory_when_its_name_is_swapped(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    authorized = sibling_directory(tmp_path, "authorized")
    attacker = sibling_directory(tmp_path, "attacker")

    def swap():
        directory.rename(authorized)
        attacker.mkdir()
        (attacker / "helpers.py").write_bytes(b"ATTACKER = 1\n")
        directory.symlink_to(attacker, target_is_directory=True)

    assert read_selected(root, directory, ["helpers.py"], before_read=swap) == {"helpers.py": b"VALUE = 41\n"}
    assert (attacker / "helpers.py").read_bytes() == b"ATTACKER = 1\n"


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_root_walk_refuses_a_symlink_standing_in_for_the_directory(tmp_path):
    root = workspace(tmp_path)
    inner = root / "project"
    inner.mkdir()
    (inner / "helpers.py").write_bytes(b"AUTHORIZED = 1\n")
    outside = sibling_directory(tmp_path, "outside")
    outside.mkdir()
    (outside / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    directory = notebook_directory(root, "project/nb.ipynb")

    inner.rename(tmp_path / "project-away")
    inner.symlink_to(outside, target_is_directory=True)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["helpers.py"])

    assert failure.value.status == 409
    assert "moved or replaced by a symlink" in str(failure.value)
    assert (outside / "helpers.py").read_bytes() == b"ATTACKER = 1\n"

@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_root_walk_refuses_a_parent_component_that_would_escape_the_root(tmp_path):
    root = workspace(tmp_path)
    (root / "inner").mkdir()
    secret = sibling_directory(tmp_path, "secret")
    secret.mkdir()
    (secret / "helpers.py").write_bytes(b"SECRET = 1\n")
    directory = root / "inner" / ".." / secret.name

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["helpers.py"])

    assert failure.value.status == 409
    assert "outside the Jupyter server root" in str(failure.value)
    assert (secret / "helpers.py").read_bytes() == b"SECRET = 1\n"


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_root_walk_refuses_an_ancestor_outside_the_root(tmp_path):
    root = workspace(tmp_path)
    outside = sibling_directory(tmp_path, "outside")
    outside.mkdir()
    (outside / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    directory = notebook_directory(root, NOTEBOOK)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(outside, directory, ["helpers.py"])

    assert failure.value.status == 409
    assert "outside the Jupyter server root" in str(failure.value)

@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_relative_open_refuses_a_name_swapped_after_every_directory_check_passed(tmp_path):
    held = sibling_directory(tmp_path, "held")
    authorized = held / "authorized"
    authorized.mkdir(parents=True)
    filled = held / "filled"
    filled.mkdir()
    (filled / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    swapped = held / "swapped"

    def swap():
        authorized.rename(swapped)
        authorized.symlink_to(filled, target_is_directory=True)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(held, authorized, ["helpers.py"], before_read=swap)

    assert failure.value.status == 409
    assert str(failure.value) == "'helpers.py' was not found beside the notebook"
    assert (filled / "helpers.py").read_bytes() == b"ATTACKER = 1\n"


@pytest.mark.skipif(not hasattr(os, "mkfifo"), reason="creating a FIFO needs POSIX os.mkfifo")
def test_read_selected_refuses_a_named_pipe(tmp_path, monkeypatch):
    def refuse_fdopen(fd, *args, **kwargs):
        raise AssertionError("a named pipe must never be read")

    monkeypatch.setattr(os, "fdopen", refuse_fdopen)
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    os.mkfifo(root / "pipe.py")

    with pytest.raises(FileSelectionError) as failure:
        read_selected(root, directory, ["pipe.py"])

    assert failure.value.status == 409
    assert "not a regular file" in str(failure.value)


def test_chosen_files_ride_the_payload_and_the_plan_summary(tmp_path):
    root = workspace(tmp_path)
    selected = read_selected(root, notebook_directory(root, NOTEBOOK), ["helpers.py", "config.json"])

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


def test_read_selected_never_returns_an_ancestor_swapped_after_the_bind(tmp_path):
    root = tmp_path / "root"
    inner = root / "work"
    inner.mkdir(parents=True)
    (inner / "helper.py").write_text("AUTHORIZED = 1\n", encoding="utf-8")

    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "helper.py").write_text("ATTACKER = 2\n", encoding="utf-8")

    directory = notebook_files.notebook_directory(root, "work/analysis.ipynb")

    def swap_ancestor():
        (root / "work").rename(tmp_path / "work-away")
        (root / "work").symlink_to(outside, target_is_directory=True)

    if notebook_files._DIRECTORY_BINDING:
        selected = notebook_files.read_selected(root, directory, ["helper.py"], before_read=swap_ancestor)
        assert selected["helper.py"] == b"AUTHORIZED = 1\n"
    else:
        with pytest.raises(notebook_files.FileSelectionError) as err:
            notebook_files.read_selected(root, directory, ["helper.py"], before_read=swap_ancestor)
        assert err.value.status == 409
    assert (outside / "helper.py").read_text(encoding="utf-8") == "ATTACKER = 2\n"


def test_read_selected_refuses_an_ancestor_swapped_before_the_bind(tmp_path):
    root = tmp_path / "root"
    inner = root / "work"
    inner.mkdir(parents=True)
    (inner / "helper.py").write_text("AUTHORIZED = 1\n", encoding="utf-8")

    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "helper.py").write_text("ATTACKER = 2\n", encoding="utf-8")

    directory = notebook_files.notebook_directory(root, "work/analysis.ipynb")
    (root / "work").rename(tmp_path / "work-away")
    (root / "work").symlink_to(outside, target_is_directory=True)

    with pytest.raises(notebook_files.FileSelectionError) as err:
        notebook_files.read_selected(root, directory, ["helper.py"])
    assert err.value.status == 409


@pytest.mark.skipif(
    notebook_files._DIRECTORY_BINDING,
    reason="the lexical refusal is the fallback path, exercised only without dir_fd binding",
)
def test_read_selected_never_returns_escaped_bytes_without_dir_fd(tmp_path):
    root = tmp_path / "root"
    inner = root / "work"
    inner.mkdir(parents=True)
    (inner / "helper.py").write_text("AUTHORIZED = 1\n", encoding="utf-8")
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "helper.py").write_text("ATTACKER = 2\n", encoding="utf-8")
    directory = notebook_files.notebook_directory(root, "work/analysis.ipynb")
    reached = []

    def swap_ancestor():
        reached.append(True)
        (root / "work").rename(tmp_path / "work-away")
        (root / "work").symlink_to(outside, target_is_directory=True)

    try:
        selected = notebook_files.read_selected(root, directory, ["helper.py"], before_read=swap_ancestor)
    except notebook_files.FileSelectionError as failure:
        assert failure.status == 409
        assert (outside / "helper.py").read_text(encoding="utf-8") == "ATTACKER = 2\n"
    else:
        assert selected["helper.py"] == b"AUTHORIZED = 1\n"
    assert reached, "the swap hook never ran, so this test proved nothing"

