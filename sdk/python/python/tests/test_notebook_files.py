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


def authorized_directory_replaced_by_a_symlink(tmp_path: Path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    authorized = tmp_path.parent / (tmp_path.name + "-authorized")
    attacker = tmp_path.parent / (tmp_path.name + "-attacker")
    directory.rename(authorized)
    attacker.mkdir()
    (attacker / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    directory.symlink_to(attacker, target_is_directory=True)
    return directory, attacker


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
            read_selected(directory, [name])


def test_read_selected_refuses_files_the_picker_skips(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)

    for name in (".hidden", "image.png"):
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
        read_selected(directory, ["huge.py"])

    assert failure.value.status == 413
    # The read asked for a bounded slice and stopped at the ceiling rather
    # than consuming the whole file.
    assert reads == [MAX_FILE_BYTES + 1]


def test_read_selected_enforces_the_payload_ceiling(tmp_path, monkeypatch):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    monkeypatch.setattr(notebook_files, "MAX_DECODED_BYTES", 10)

    with pytest.raises(FileSelectionError) as failure:
        read_selected(directory, ["helpers.py", "config.json"])
    assert failure.value.status == 413
    assert "payload ceiling" in str(failure.value)


def test_read_selected_refuses_a_directory_swapped_for_a_symlink(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    assert read_selected(directory, ["helpers.py"], before_read=lambda: None) == {"helpers.py": b"VALUE = 41\n"}

    authorized = tmp_path.parent / (tmp_path.name + "-authorized")
    directory.rename(authorized)
    attacker = tmp_path.parent / (tmp_path.name + "-attacker")
    attacker.mkdir()
    (attacker / "helpers.py").write_bytes(b"ATTACKER = 1\n")
    directory.symlink_to(attacker, target_is_directory=True)

    with pytest.raises(FileSelectionError):
        read_selected(directory, ["helpers.py"])
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
        read_selected(directory, ["helpers.py"])
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
        read_selected(directory, ["helpers.py"], before_read=swap)


def test_read_selected_refuses_an_alias_created_after_selection(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    secret = tmp_path / "outside_secret.txt"
    secret.write_bytes(b"SA-TOKEN-abcdef")

    def create_alias():
        (root / "late.py").symlink_to(secret)

    with pytest.raises(FileSelectionError):
        read_selected(directory, ["late.py"], before_read=create_alias)


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_read_selected_binds_to_the_opened_directory_when_its_name_is_swapped(tmp_path):
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    authorized = tmp_path.parent / (tmp_path.name + "-authorized")
    attacker = tmp_path.parent / (tmp_path.name + "-attacker")

    def swap():
        directory.rename(authorized)
        attacker.mkdir()
        (attacker / "helpers.py").write_bytes(b"ATTACKER = 1\n")
        directory.symlink_to(attacker, target_is_directory=True)

    assert read_selected(directory, ["helpers.py"], before_read=swap) == {"helpers.py": b"VALUE = 41\n"}
    assert (attacker / "helpers.py").read_bytes() == b"ATTACKER = 1\n"


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_directory_open_refuses_a_symlink_without_relying_on_any_later_layer(tmp_path, monkeypatch):
    directory, attacker = authorized_directory_replaced_by_a_symlink(tmp_path)
    monkeypatch.setattr(notebook_files, "_verify_authorized_directory", lambda opened: None)
    monkeypatch.setattr(notebook_files, "_read_names", lambda opened, names: {"helpers.py": b"unreached"})

    with pytest.raises(FileSelectionError) as failure:
        read_selected(directory, ["helpers.py"])

    assert failure.value.status == 409
    assert str(failure.value).startswith("the notebook directory was replaced by a symlink before it could be read")
    assert isinstance(failure.value.__cause__, OSError)
    assert (attacker / "helpers.py").read_bytes() == b"ATTACKER = 1\n"


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_directory_identity_refuses_a_symlink_the_open_flags_allowed(tmp_path, monkeypatch):
    directory, attacker = authorized_directory_replaced_by_a_symlink(tmp_path)
    monkeypatch.setattr(notebook_files, "_DIRECTORY_OPEN_FLAGS", os.O_RDONLY)
    monkeypatch.setattr(notebook_files, "_read_names", lambda opened, names: {"helpers.py": b"unreached"})

    with pytest.raises(FileSelectionError) as failure:
        read_selected(directory, ["helpers.py"])

    assert failure.value.status == 409
    assert str(failure.value).startswith("the notebook directory was replaced by a symlink before it could be read")
    assert (attacker / "helpers.py").read_bytes() == b"ATTACKER = 1\n"


@pytest.mark.skipif(not notebook_files._DIRECTORY_BINDING, reason="relative opens need POSIX dir_fd and O_NOFOLLOW")
def test_relative_open_refuses_a_name_swapped_after_every_directory_check_passed(tmp_path):
    held = tmp_path.parent / (tmp_path.name + "-held")
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
        read_selected(authorized, ["helpers.py"], before_read=swap)

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
        read_selected(directory, ["pipe.py"])

    assert failure.value.status == 409
    assert "not a regular file" in str(failure.value)


def test_pathname_read_refuses_a_named_pipe_without_the_directory_handle(tmp_path, monkeypatch):
    if not hasattr(os, "mkfifo"):
        pytest.skip("creating a FIFO needs POSIX os.mkfifo")
    if not os.path.exists("C:\\Windows"):
        pytest.skip("the POSIX pathname branch blocks on a FIFO open and is only exercised on Windows")

    def refuse_fdopen(fd, *args, **kwargs):
        raise AssertionError("a named pipe must never be read")

    monkeypatch.setattr(os, "fdopen", refuse_fdopen)
    monkeypatch.setattr(notebook_files, "_DIRECTORY_BINDING", False)
    root = workspace(tmp_path)
    directory = notebook_directory(root, NOTEBOOK)
    os.mkfifo(root / "pipe.py")

    with pytest.raises(FileSelectionError) as failure:
        read_selected(directory, ["pipe.py"])

    assert failure.value.status == 409
    assert "not a regular file" in str(failure.value)


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
