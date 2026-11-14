# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import errno
import fcntl
import functools
import hashlib
import http.server
import io
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import types
import urllib.error
from collections import namedtuple
from pathlib import Path

import pytest

from tau import datasets, stellar

SDK_ROOT = Path(datasets.__file__).resolve().parents[1]

class _RecordingSink:
    def __init__(self) -> None:
        self.calls: list[tuple[dict[str, float], int]] = []

    def __call__(self, metrics, step: int) -> None:
        self.calls.append((dict(metrics), step))

class _Waiter(threading.Thread):
    def __init__(self, **kwargs) -> None:
        super().__init__(daemon=True)
        self.kwargs = kwargs
        self.result: Path | None = None
        self.error: BaseException | None = None

    def run(self) -> None:
        try:
            self.result = datasets.stage(**self.kwargs)
        except Exception as exc:
            self.error = exc

def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()

def _source(root: Path, rel: str, data: bytes) -> tuple[str, str]:
    path = root / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return path.as_uri(), _sha256(data)

def _leftovers(root: Path) -> list[str]:
    return sorted(path.name for path in root.rglob("*") if path.name.endswith(".tau-partial"))

@pytest.fixture(autouse=True)
def fast_stage(monkeypatch) -> None:
    monkeypatch.setattr(datasets, "_RETRY_BACKOFF_SECONDS", 0.0)
    monkeypatch.setattr(datasets, "_POLL_SECONDS", 0.01)

@pytest.fixture
def no_active_run(monkeypatch) -> None:
    monkeypatch.setattr(stellar, "_ACTIVE_RUN", None)

def _forbid_source_reads(monkeypatch) -> None:
    def fail(uri: str):
        raise AssertionError(f"source must not be read: {uri}")

    monkeypatch.setattr(datasets, "_open_source", fail)

def test_stage_downloads_file_sources_with_relative_layout(tmp_path):
    first_uri, first_sha = _source(tmp_path / "src", "train/000.bin", b"a" * 1000)
    second_uri, second_sha = _source(tmp_path / "src", "val/000.bin", b"b" * 24)
    sink = _RecordingSink()

    staged = datasets.stage(
        uris=[first_uri, second_uri],
        sha256s=[first_sha, second_sha],
        dest=tmp_path / "dest",
        per="none",
        log=sink,
    )

    assert staged == tmp_path / "dest"
    assert (staged / "train" / "000.bin").read_bytes() == b"a" * 1000
    assert (staged / "val" / "000.bin").read_bytes() == b"b" * 24
    assert _leftovers(staged) == []
    assert len(sink.calls) == 1
    metrics, step = sink.calls[0]
    assert step == 0
    assert metrics["data/stage_bytes"] == 1024
    assert metrics["data/stage_skipped_bytes"] == 0
    assert metrics["data/stage_seconds"] >= 0

def test_stage_accepts_plain_local_paths_and_sha256_prefix(tmp_path):
    path = tmp_path / "src" / "shard.bin"
    path.parent.mkdir()
    path.write_bytes(b"local")

    staged = datasets.stage(uris=[str(path)], sha256s=["sha256:" + _sha256(b"local").upper()], dest=tmp_path / "dest", per="none")

    assert (staged / "shard.bin").read_bytes() == b"local"

@pytest.mark.parametrize("prefix", [None, "TRAINING_DATA"])
def test_stage_reads_dataset_ref_env_contract(tmp_path, monkeypatch, prefix):
    env_prefix = prefix or "FINEWEB_DATASET"
    first_uri, first_sha = _source(tmp_path / "staged", "sample/000.bin", b"0" * 8)
    second_uri, second_sha = _source(tmp_path / "staged", "sample/001.bin", b"1" * 8)
    monkeypatch.setenv(f"{env_prefix}_URIS", f"{first_uri},{second_uri}")
    monkeypatch.setenv(f"{env_prefix}_SHA256S", f"{first_sha},{second_sha}")
    monkeypatch.setenv(f"{env_prefix}_TOKEN_COUNTS", "4,4")

    staged = datasets.stage(env_prefix=prefix, dest=tmp_path / "dest", per="none")

    assert sorted(path.name for path in staged.iterdir()) == ["000.bin", "001.bin"]

def test_stage_env_contract_requires_matching_lengths(tmp_path, monkeypatch):
    uri, sha = _source(tmp_path, "a.bin", b"a")
    monkeypatch.setenv("TRAINING_DATA_URIS", f"{uri},{uri}")
    monkeypatch.setenv("TRAINING_DATA_SHA256S", sha)

    with pytest.raises(ValueError, match="TRAINING_DATA_SHA256S has 1 entries but TRAINING_DATA_URIS has 2"):
        datasets.stage(env_prefix="TRAINING_DATA", dest=tmp_path / "dest")

def test_stage_env_contract_requires_uris(tmp_path, monkeypatch):
    monkeypatch.delenv("FINEWEB_DATASET_URIS", raising=False)

    with pytest.raises(ValueError, match="FINEWEB_DATASET_URIS lists no files"):
        datasets.stage(dest=tmp_path / "dest")

def test_stage_defaults_destination_to_tau_datasets_dir(tmp_path, monkeypatch):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"data")
    monkeypatch.setenv("TAU_DATASETS_DIR", str(tmp_path / "datasets"))

    staged = datasets.stage("corpus@v3", uris=[uri], sha256s=[sha], per="none")

    assert staged == tmp_path / "datasets" / "corpus" / "v3"
    assert (staged / "shard.bin").read_bytes() == b"data"

def test_stage_falls_back_to_tau_hot_dir(tmp_path, monkeypatch):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"data")
    monkeypatch.delenv("TAU_DATASETS_DIR", raising=False)
    monkeypatch.setenv("TAU_HOT_DIR", str(tmp_path / "hot"))

    staged = datasets.stage(name="corpus", version="v1", uris=[uri], sha256s=[sha], per="none")

    assert staged == tmp_path / "hot" / "datasets" / "corpus" / "v1"

@pytest.mark.parametrize(
    ("kwargs", "message"),
    [
        ({"ref": "corpus"}, "NAME@VERSION"),
        ({"ref": "corpus@v1", "version": "v2"}, "conflicts"),
        ({"name": "../escape", "version": "v1"}, "single path component"),
        ({}, "needs ref="),
    ],
)
def test_stage_validates_destination_identity(tmp_path, kwargs, message):
    uri, sha = _source(tmp_path, "a.bin", b"a")

    with pytest.raises(ValueError, match=message):
        datasets.stage(uris=[uri], sha256s=[sha], **kwargs)

def test_stage_rejects_unsupported_scheme(tmp_path):
    with pytest.raises(ValueError, match="unsupported dataset source s3://bucket/a.bin"):
        datasets.stage(uris=["s3://bucket/a.bin?sig=secret"], sha256s=[_sha256(b"a")], dest=tmp_path)

def test_stage_skips_existing_file_with_matching_hash(tmp_path, monkeypatch):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"cached" * 10)
    dest = tmp_path / "dest"
    dest.mkdir()
    (dest / "shard.bin").write_bytes(b"cached" * 10)
    _forbid_source_reads(monkeypatch)
    sink = _RecordingSink()

    staged = datasets.stage(uris=[uri], sha256s=[sha], dest=dest, per="none", log=sink)

    assert (staged / "shard.bin").read_bytes() == b"cached" * 10
    assert sink.calls[0][0]["data/stage_bytes"] == 0
    assert sink.calls[0][0]["data/stage_skipped_bytes"] == 60

def test_stage_replaces_existing_file_with_wrong_hash(tmp_path):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"fresh")
    dest = tmp_path / "dest"
    dest.mkdir()
    (dest / "shard.bin").write_bytes(b"stale")

    datasets.stage(uris=[uri], sha256s=[sha], dest=dest, per="none")

    assert (dest / "shard.bin").read_bytes() == b"fresh"

def test_stage_hash_mismatch_raises_and_leaves_no_final_file(tmp_path):
    uri, _ = _source(tmp_path / "src", "shard.bin", b"tampered")
    dest = tmp_path / "dest"

    with pytest.raises(datasets.StageError, match="sha256 mismatch for shard.bin"):
        datasets.stage(uris=[uri], sha256s=[_sha256(b"expected")], dest=dest, per="none")

    assert not (dest / "shard.bin").exists()
    assert _leftovers(dest) == []

def test_stage_missing_source_is_not_retried(tmp_path, monkeypatch):
    opened: list[str] = []
    real_open = datasets._open_source

    def counting_open(uri: str):
        opened.append(uri)
        return real_open(uri)

    monkeypatch.setattr(datasets, "_open_source", counting_open)

    with pytest.raises(datasets.StageError, match="missing.bin .* after 1 attempt"):
        datasets.stage(uris=[(tmp_path / "missing.bin").as_uri()], sha256s=[_sha256(b"x")], dest=tmp_path / "dest", per="none", retries=3)

    assert len(opened) == 1

class _BrokenStream(io.BytesIO):
    def __init__(self, data: bytes, fail_after: int) -> None:
        super().__init__(data)
        self.fail_after = fail_after

    def read(self, size: int = -1) -> bytes:
        if self.tell() >= self.fail_after:
            raise ConnectionResetError("connection reset mid-stream")
        return super().read(size)

def _flaky_opener(data: bytes, failures: int, dest: Path, observed: list[bool]):
    calls = {"count": 0}

    def open_source(uri: str):
        calls["count"] += 1
        observed.append((dest / "shard.bin").exists())
        if calls["count"] <= failures:
            return _BrokenStream(data, fail_after=len(data) // 2)
        return io.BytesIO(data)

    return open_source, calls

def test_stage_partial_download_leaves_no_final_file(tmp_path, monkeypatch):
    data = os.urandom(4096)
    dest = tmp_path / "dest"
    observed: list[bool] = []
    opener, calls = _flaky_opener(data, failures=10, dest=dest, observed=observed)
    monkeypatch.setattr(datasets, "_open_source", opener)
    monkeypatch.setattr(datasets, "_CHUNK_BYTES", 512)

    with pytest.raises(datasets.StageError, match="after 2 attempt.*connection reset"):
        datasets.stage(uris=["https://example.test/shard.bin"], sha256s=[_sha256(data)], dest=dest, per="none", retries=1)

    assert calls["count"] == 2
    assert not (dest / "shard.bin").exists()
    assert _leftovers(dest) == []

def test_stage_retries_partial_download_until_complete(tmp_path, monkeypatch):
    data = os.urandom(4096)
    dest = tmp_path / "dest"
    observed: list[bool] = []
    opener, calls = _flaky_opener(data, failures=2, dest=dest, observed=observed)
    monkeypatch.setattr(datasets, "_open_source", opener)
    monkeypatch.setattr(datasets, "_CHUNK_BYTES", 512)
    sink = _RecordingSink()

    datasets.stage(uris=["https://example.test/shard.bin?sig=x"], sha256s=[_sha256(data)], dest=dest, per="none", retries=2, log=sink)

    assert calls["count"] == 3
    assert observed == [False, False, False]
    assert (dest / "shard.bin").read_bytes() == data
    assert _leftovers(dest) == []
    assert sink.calls[0][0]["data/stage_bytes"] == len(data)

@pytest.mark.parametrize("code", [errno.ENOSPC, errno.EDQUOT])
def test_stage_reports_full_or_over_quota_volume(tmp_path, monkeypatch, code):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"payload")
    dest = tmp_path / "dest"
    real_open = Path.open

    class _FullWriter(io.BytesIO):
        def write(self, data) -> int:
            raise OSError(code, os.strerror(code))

    def open_path(self, mode="r", *args, **kwargs):
        if mode == "wb" and self.name.endswith(".tau-partial"):
            real_open(self, mode, *args, **kwargs).close()
            return _FullWriter()
        return real_open(self, mode, *args, **kwargs)

    monkeypatch.setattr(Path, "open", open_path)

    with pytest.raises(datasets.StageError, match="no space left writing shard.bin.*storage.hot_size"):
        datasets.stage(uris=[uri], sha256s=[sha], dest=dest, per="none", retries=3)

    assert not (dest / "shard.bin").exists()
    assert _leftovers(dest) == []

def test_stage_space_check_fails_before_downloading(tmp_path, monkeypatch):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"x" * 100)
    _forbid_source_reads(monkeypatch)
    usage = namedtuple("usage", "total used free")
    monkeypatch.setattr(datasets.shutil, "disk_usage", lambda path: usage(1000, 990, 10))

    with pytest.raises(datasets.StageError, match="not enough space to stage corpus@v1: 100 bytes needed, 10 free.*storage.hot_size"):
        datasets.stage("corpus@v1", uris=[uri], sha256s=[sha], dest=tmp_path / "dest", per="none")

def test_stage_space_check_uses_explicit_sizes(tmp_path, monkeypatch):
    _forbid_source_reads(monkeypatch)
    usage = namedtuple("usage", "total used free")
    monkeypatch.setattr(datasets.shutil, "disk_usage", lambda path: usage(1000, 0, 1000))

    with pytest.raises(datasets.StageError, match="2048 bytes needed, 1000 free"):
        datasets.stage(
            uris=["https://example.test/a.bin", "https://example.test/b.bin"],
            sha256s=[_sha256(b"a"), _sha256(b"b")],
            sizes=[1024, 1024],
            dest=tmp_path / "dest",
            per="none",
        )

def test_stage_space_check_uses_content_length(tmp_path, monkeypatch):
    class _Response(io.BytesIO):
        headers = {"Content-Length": "5000"}

    monkeypatch.setattr(datasets, "_open_source", lambda uri: _Response(b"x"))
    usage = namedtuple("usage", "total used free")
    monkeypatch.setattr(datasets.shutil, "disk_usage", lambda path: usage(1000, 0, 1000))

    with pytest.raises(datasets.StageError, match="not enough space to stage a.bin: 5000 bytes needed"):
        datasets.stage(uris=["https://example.test/a.bin"], sha256s=[_sha256(b"x")], dest=tmp_path / "dest", per="none")

    assert _leftovers(tmp_path / "dest") == []

class _QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, format: str, *args) -> None:
        pass

@pytest.mark.parametrize(("retries", "requests"), [(0, 1), (1, 2)])
def test_stage_retries_truncated_http_response(tmp_path, retries, requests):
    payload = b"0123456789"
    served: list[str] = []

    class _Truncating(_QuietHandler):
        def do_GET(self) -> None:
            served.append(self.path)
            self.send_response(200)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload[:3] if len(served) == 1 else payload)
            self.close_connection = True

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Truncating)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    dest = tmp_path / "dest"
    uri = f"http://127.0.0.1:{server.server_address[1]}/shard.bin"
    try:
        if retries:
            datasets.stage(uris=[uri], sha256s=[_sha256(payload)], dest=dest, per="none", retries=retries)
        else:
            with pytest.raises(datasets.StageError, match="after 1 attempt.*response ended after 3 of 10 bytes"):
                datasets.stage(uris=[uri], sha256s=[_sha256(payload)], dest=dest, per="none", retries=retries)
    finally:
        server.shutdown()
        server.server_close()

    assert len(served) == requests
    assert ((dest / "shard.bin").read_bytes() == payload) if retries else not (dest / "shard.bin").exists()

def test_stage_downloads_over_http(tmp_path):
    root = tmp_path / "served"
    (root / "corpus").mkdir(parents=True)
    (root / "corpus" / "000.bin").write_bytes(b"over the wire")
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(_QuietHandler, directory=str(root)))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}/corpus"
        staged = datasets.stage(uris=[f"{base}/000.bin"], sha256s=[_sha256(b"over the wire")], dest=tmp_path / "dest", per="none")
    finally:
        server.shutdown()
        server.server_close()

    assert (staged / "000.bin").read_bytes() == b"over the wire"

@pytest.mark.parametrize(
    ("exc", "transient"),
    [
        (lambda: urllib.error.HTTPError("u", 503, "unavailable", {}, None), True),
        (lambda: urllib.error.HTTPError("u", 429, "slow down", {}, None), True),
        (lambda: urllib.error.HTTPError("u", 404, "missing", {}, None), False),
        (lambda: urllib.error.URLError("dns"), True),
        (TimeoutError, True),
        (FileNotFoundError, False),
    ],
)
def test_stage_retries_only_transient_errors(exc, transient):
    error = exc()
    try:
        assert datasets._is_transient(error) is transient
    finally:
        if isinstance(error, urllib.error.HTTPError):
            error.close()

def test_stage_closes_http_errors_it_retries(tmp_path, monkeypatch):
    errors = [urllib.error.HTTPError("u", 503, "unavailable", {}, io.BytesIO()) for _ in range(2)]
    attempts = iter(errors)

    def fail(file, stop):
        raise next(attempts)

    monkeypatch.setattr(datasets, "_download", fail)
    monkeypatch.setattr(datasets, "_RETRY_BACKOFF_SECONDS", 0.0)
    file = datasets._StageFile("https://example.invalid/a.bin", "0" * 64, "a.bin", tmp_path / "a.bin", None)
    with pytest.raises(datasets.StageError, match="after 2 attempt"):
        datasets._fetch(file, "corpus@v1", 1, threading.Event())
    assert all(error.fp.closed for error in errors)

def _hold_lock(dest: Path):
    dest.mkdir(parents=True, exist_ok=True)
    handle = (dest / ".tau-stage.lock").open("a")
    fcntl.flock(handle, fcntl.LOCK_EX)
    return handle

def test_stage_per_node_waits_for_the_lock_holder(tmp_path):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"node" * 100)
    dest = tmp_path / "dest"
    holder = _hold_lock(dest)
    sink = _RecordingSink()
    waiter = _Waiter(uris=[uri], sha256s=[sha], dest=dest, timeout=10, log=sink)
    waiter.start()
    time.sleep(0.05)
    assert waiter.is_alive()
    assert not (dest / "shard.bin").exists()

    holder.close()
    waiter.join(10)

    assert waiter.error is None
    assert waiter.result == dest
    assert (dest / "shard.bin").read_bytes() == b"node" * 100
    assert sink.calls[0][0]["data/stage_bytes"] == 400

def test_stage_per_node_times_out_while_the_lock_is_held(tmp_path):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"current")
    dest = tmp_path / "dest"
    holder = _hold_lock(dest)
    try:
        with pytest.raises(datasets.StageError, match="timed out after 0.2s waiting for another process to stage"):
            datasets.stage(uris=[uri], sha256s=[sha], dest=dest, timeout=0.2)
    finally:
        holder.close()

    assert not (dest / "shard.bin").exists()

def test_stage_per_node_trusts_unchanged_completed_files(tmp_path, monkeypatch):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"cached")
    dest = tmp_path / "dest"
    datasets.stage(uris=[uri], sha256s=[sha], dest=dest)
    _forbid_source_reads(monkeypatch)
    monkeypatch.setattr(datasets, "_staged_size", lambda file, stop: pytest.fail("completed files must not be rehashed"))
    sink = _RecordingSink()

    assert datasets.stage(uris=[uri], sha256s=[sha], dest=dest, log=sink) == dest

    assert sink.calls[0][0]["data/stage_bytes"] == 0
    assert sink.calls[0][0]["data/stage_skipped_bytes"] == 6

def test_stage_per_node_restages_files_changed_after_completion(tmp_path):
    first_uri, first_sha = _source(tmp_path / "src", "a.bin", b"first")
    second_uri, second_sha = _source(tmp_path / "src", "b.bin", b"second")
    dest = tmp_path / "dest"
    datasets.stage(uris=[first_uri, second_uri], sha256s=[first_sha, second_sha], dest=dest)
    (dest / "a.bin").unlink()
    (dest / "b.bin").write_bytes(b"SECOND")
    sink = _RecordingSink()

    datasets.stage(uris=[first_uri, second_uri], sha256s=[first_sha, second_sha], dest=dest, log=sink)

    assert (dest / "a.bin").read_bytes() == b"first"
    assert (dest / "b.bin").read_bytes() == b"second"
    assert sink.calls[0][0]["data/stage_bytes"] == 11

def test_stage_per_node_ignores_marker_for_other_file_list(tmp_path):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"current")
    dest = tmp_path / "dest"
    dest.mkdir()
    (dest / "shard.bin").write_bytes(b"stale!!")
    stamp = datasets._file_stamp(dest / "shard.bin")
    (dest / ".tau-stage.json").write_text(json.dumps({"manifest": "0" * 64, "files": {"shard.bin": stamp}}))

    datasets.stage(uris=[uri], sha256s=[sha], dest=dest)

    assert (dest / "shard.bin").read_bytes() == b"current"

def test_stage_per_node_reports_its_own_error_after_another_process_failed(tmp_path):
    uri, _ = _source(tmp_path / "src", "shard.bin", b"corrupt")
    dest = tmp_path / "dest"
    expected = _sha256(b"expected")
    for _ in range(2):
        with pytest.raises(datasets.StageError, match="sha256 mismatch for shard.bin"):
            datasets.stage(uris=[uri], sha256s=[expected], dest=dest, timeout=0.5)
    assert not (dest / ".tau-stage.json").exists()

    (tmp_path / "src" / "shard.bin").write_bytes(b"expected")
    datasets.stage(uris=[uri], sha256s=[expected], dest=dest, timeout=0.5)

    assert (dest / "shard.bin").read_bytes() == b"expected"

def test_stage_per_node_takes_over_from_a_killed_lock_holder(tmp_path):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"survivor")
    dest = tmp_path / "dest"
    dest.mkdir()
    code = f"""
import fcntl, time
handle = open({str(dest / ".tau-stage.lock")!r}, "a")
fcntl.flock(handle, fcntl.LOCK_EX)
print("locked", flush=True)
time.sleep(60)
"""
    holder = subprocess.Popen([sys.executable, "-c", code], stdout=subprocess.PIPE, text=True)
    try:
        assert holder.stdout.readline().strip() == "locked"
        (dest / ".shard.bin.dead.tau-partial").write_bytes(b"half")
        holder.kill()
        holder.wait(10)

        datasets.stage(uris=[uri], sha256s=[sha], dest=dest, timeout=10)
    finally:
        holder.kill()
        holder.wait(10)
        holder.stdout.close()

    assert (dest / "shard.bin").read_bytes() == b"survivor"
    assert _leftovers(dest) == []

def test_stage_per_node_downloads_once_across_processes(tmp_path):
    data = b"multi-process" * 1000
    uri, sha = _source(tmp_path / "src", "shard.bin", data)
    dest = tmp_path / "dest"
    code = f"""
import json
from tau import datasets
calls = []
datasets._POLL_SECONDS = 0.01
path = datasets.stage(uris=[{uri!r}], sha256s=[{sha!r}], dest={str(dest)!r}, timeout=30, log=lambda m, s: calls.append(dict(m)))
print(json.dumps({{"path": str(path), "metrics": calls}}))
"""
    env = {**os.environ, "PYTHONPATH": str(SDK_ROOT)}
    processes = [
        subprocess.Popen([sys.executable, "-c", code], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        for _ in range(3)
    ]
    outputs = [process.communicate(timeout=60) for process in processes]

    results = []
    for process, (stdout, stderr) in zip(processes, outputs):
        assert process.returncode == 0, stderr
        results.append(json.loads(stdout))
    assert all(result["path"] == str(dest) for result in results)
    assert sorted(result["metrics"][0]["data/stage_bytes"] for result in results) == [0, 0, len(data)]
    assert (dest / "shard.bin").read_bytes() == data

def test_stage_writes_to_active_stellar_run(tmp_path, no_active_run):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"metrics")
    run = stellar.init(project="nanogpt", run="stage", dir=tmp_path / "runs")
    try:
        datasets.stage(uris=[uri], sha256s=[sha], dest=tmp_path / "dest", per="none")
    finally:
        stellar.finish()

    rows = [json.loads(line) for line in run.history_path.read_text().splitlines()]
    assert len(rows) == 1
    assert rows[0]["data/stage_bytes"] == 7
    assert rows[0]["data/stage_skipped_bytes"] == 0
    assert rows[0]["data/stage_seconds"] >= 0

def test_stage_imports_without_stellar_module(tmp_path):
    shim = tmp_path / "tau"
    shim.mkdir()
    shutil.copy(Path(datasets.__file__), shim / "datasets.py")
    (shim / "__init__.py").write_text("from . import datasets as datasets\n", encoding="utf-8")
    uri, sha = _source(tmp_path / "src", "shard.bin", b"shim")
    code = f"""
import sys
sys.path.insert(0, {str(tmp_path)!r})
import tau
path = tau.datasets.stage(uris=[{uri!r}], sha256s=[{sha!r}], dest={str(tmp_path / "dest")!r}, per="none")
print((path / "shard.bin").read_text())
"""

    res = subprocess.run([sys.executable, "-S", "-c", code], capture_output=True, text=True, check=False)

    assert res.returncode == 0, res.stderr
    assert res.stdout.strip() == "shim"

def test_stage_works_when_exec_into_cluster_shim_module(tmp_path):
    uri, sha = _source(tmp_path / "src", "shard.bin", b"cluster")
    module = types.ModuleType("tau.datasets")
    exec(Path(datasets.__file__).read_text(encoding="utf-8"), module.__dict__)

    staged = module.stage(uris=[uri], sha256s=[sha], dest=tmp_path / "dest", per="none", log=_RecordingSink())

    assert (staged / "shard.bin").read_bytes() == b"cluster"
