# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Dataset helpers for Tau workloads: portable file references, hot-volume staging, and data-loading timing."""

from __future__ import annotations

import contextlib
import errno
import hashlib
import http.client
import json
import os
import random
import shutil
import threading
import urllib.error
import urllib.parse
import urllib.request
import uuid
from collections.abc import Callable, Iterable, Iterator, Mapping, Sequence
from concurrent.futures import FIRST_EXCEPTION, Future, ThreadPoolExecutor, wait
from pathlib import Path, PurePosixPath, PureWindowsPath
from time import monotonic, perf_counter, sleep
from typing import BinaryIO, NamedTuple, TypeVar

try:
    import fcntl
except ImportError:
    fcntl = None

_T = TypeVar("_T")

_MetricsSink = Callable[[Mapping[str, float], int], object]

_DEFAULT_ENV_PREFIX = "FINEWEB_DATASET"
_STAGE_MARKER = ".tau-stage.json"
_STAGE_LOCK = ".tau-stage.lock"
_PARTIAL_SUFFIX = ".tau-partial"
_CHUNK_BYTES = 8 * 1024 * 1024
_REQUEST_TIMEOUT_SECONDS = 120.0
_RETRY_BACKOFF_SECONDS = 1.0
_POLL_SECONDS = 1.0
_SPACE_ERRNOS = frozenset(code for code in (errno.ENOSPC, getattr(errno, "EDQUOT", None)) if code is not None)

def dataset_file_reference(root: str | Path, raw_path: str, *, field_name: str = "path") -> tuple[str, Path]:
    """Return a portable dataset-relative path and resolved local path.

    Dataset CSVs and manifests should carry paths relative to the mounted
    dataset root. This helper normalizes those records to POSIX separators while
    rejecting absolute paths or references that resolve outside ``root``.
    """

    rel_input = str(raw_path).strip()
    if not rel_input:
        raise ValueError(f"{field_name} must reference a file under dataset root: {raw_path!r}")

    posix_input = PurePosixPath(rel_input)
    windows_input = PureWindowsPath(rel_input)
    if (
        posix_input.is_absolute()
        or windows_input.is_absolute()
        or bool(windows_input.drive)
        or bool(windows_input.root)
    ):
        raise ValueError(f"{field_name} must be relative to dataset root: {raw_path!r}")

    normalized = PurePosixPath(rel_input.replace("\\", "/"))
    resolved_root = Path(root).expanduser().resolve(strict=False)
    candidate = (resolved_root / Path(*normalized.parts)).resolve(strict=False)
    if not candidate.is_relative_to(resolved_root):
        raise ValueError(f"{field_name} escapes dataset root: {raw_path!r}")

    rel_path = candidate.relative_to(resolved_root).as_posix()
    if rel_path == ".":
        raise ValueError(f"{field_name} must reference a file under dataset root: {raw_path!r}")
    return rel_path, candidate

def timed(
    iterable: Iterable[_T],
    name: str = "data",
    *,
    log: _MetricsSink | None = None,
    start_step: int = 0,
) -> TimedIterator[_T]:
    """Iterate ``iterable`` and log per-step data-loading time to Stellar.

    Each item is one step. For step ``n`` the wrapper logs
    ``{name}/wait_seconds``, the time blocked fetching item ``n``, and
    ``{name}/step_seconds``, the time from requesting item ``n`` to requesting
    item ``n + 1``, so step time includes the wait. The final step of a pass
    also carries ``{name}/epoch_wait_seconds`` and ``{name}/epoch_seconds``.
    Steps are numbered from ``start_step``.

    Metrics go to ``log(metrics, step)`` when given, otherwise to the active
    ``tau.stellar`` run. With neither, the iterable passes through untimed.

    The final row is written when the iterable is exhausted or the returned
    iterator is closed. Closing it also closes the source iterator when it has
    a ``close()`` method, releasing loader workers, files, or streams. A
    ``for`` loop that breaks does not close it, so use
    ``with timed(...) as batches:`` or call ``close()`` after an early exit.
    """

    if not name:
        raise ValueError("timed name is required")
    source = iter(iterable)
    sink = log if log is not None else _active_run_sink()
    if sink is None:
        return TimedIterator(source)
    return TimedIterator(_timed(source, name, sink, int(start_step)), source)

class TimedIterator(Iterator[_T]):
    """Iterator returned by :func:`timed`; closing it writes the final row and closes the source."""

    def __init__(self, iterator: Iterator[_T], source: Iterator[_T] | None = None) -> None:
        self._iterator = iterator
        self._source = source

    def __next__(self) -> _T:
        return next(self._iterator)

    def close(self) -> None:
        try:
            _close(self._iterator)
        finally:
            if self._source is not None:
                _close(self._source)

    def __enter__(self) -> TimedIterator[_T]:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

class StageError(RuntimeError):
    """Raised when :func:`stage` cannot fetch, verify, store, or wait for a dataset."""

def stage(
    ref: str | None = None,
    *,
    env_prefix: str | None = None,
    uris: Sequence[str] | None = None,
    sha256s: Sequence[str] | None = None,
    sizes: Sequence[int] | None = None,
    name: str | None = None,
    version: str | None = None,
    dest: str | os.PathLike[str] | None = None,
    per: str = "node",
    max_workers: int = 8,
    retries: int = 3,
    timeout: float = 3600.0,
    log: _MetricsSink | None = None,
) -> Path:
    """Download and verify a dataset's files onto the pod's hot volume.

    Sources default to the ``tau data dataset ref -o env`` contract:
    comma-separated ``{env_prefix}_URIS`` and ``{env_prefix}_SHA256S``, with
    ``env_prefix`` defaulting to the CLI's ``FINEWEB_DATASET``. Pass ``uris=``
    and ``sha256s=`` to stage an explicit list instead, and ``sizes=`` when
    byte counts are known. Sources may be ``file://``, ``https://``, or
    ``http://`` URIs or local paths. Files keep their paths relative to the
    sources' common directory.

    Files land in ``dest``, which defaults to
    ``$TAU_DATASETS_DIR/<name>/<version>`` (falling back to
    ``$TAU_HOT_DIR/datasets`` and then ``/mnt/datasets``). ``ref`` is a
    ``NAME@VERSION`` string naming that directory; it is not resolved against
    the dataset registry. The staged directory is returned.

    Up to ``max_workers`` files download in parallel, each retried up to
    ``retries`` times with backoff on transient network errors, including a
    response that ends before its ``Content-Length``. Every file is
    written under a temporary name, hashed while it streams, and renamed into
    place only when its SHA-256 matches, so a partial or corrupt download never
    appears under its final name. Files already present with the expected hash
    are kept without reading the source. When sizes are known from ``sizes=``,
    local sources, or ``Content-Length``, free space is checked before writing.
    A hash mismatch, a full or over-quota volume, or an exhausted retry budget
    raises :class:`StageError`.

    With ``per="node"`` the processes staging into ``dest`` take turns holding
    an exclusive lock on it. The first stages the files and writes a
    completion marker recording each file's size, modification time, and
    inode. Each later process accepts the marker only if every file still
    matches it, and otherwise stages the files itself. So a node downloads
    each file once, and files that went missing or changed, or that a failed
    process never finished, are staged again. A process raises
    :class:`StageError` if it can't take the lock within ``timeout`` seconds.
    The lock is released when its holder exits, even on a crash.
    ``per="none"`` stages in every calling process without coordination.

    Each call logs ``data/stage_seconds`` (including any wait for the lock),
    ``data/stage_bytes`` (bytes it downloaded), and ``data/stage_skipped_bytes``
    (bytes already present) to ``log(metrics, 0)`` when given, otherwise to
    the active ``tau.stellar`` run.
    """

    if per not in ("node", "none"):
        raise ValueError(f"per must be 'node' or 'none', got {per!r}")
    if max_workers < 1:
        raise ValueError(f"max_workers must be at least 1, got {max_workers}")
    if retries < 0:
        raise ValueError(f"retries must be non-negative, got {retries}")
    name, version = _stage_identity(ref, name, version)
    uri_list, sha_list = _stage_sources(env_prefix, uris, sha256s)
    size_list = _stage_sizes(sizes, len(uri_list))
    root = Path(dest).expanduser() if dest is not None else _stage_dest(name, version)
    files = _stage_plan(root, uri_list, sha_list, size_list)
    label = f"{name}@{version}" if name and version else str(root)
    if per == "node" and fcntl is None:
        raise StageError("per='node' needs POSIX file locks; use per='none' on this platform")

    started = perf_counter()
    root.mkdir(parents=True, exist_ok=True)
    if per == "node":
        downloaded, skipped = _stage_per_node(root, files, label, max_workers, retries, timeout)
    else:
        downloaded, skipped = _stage_all(root, files, label, max_workers, retries)
    sink = log if log is not None else _active_run_sink()
    if sink is not None:
        sink(
            {
                "data/stage_seconds": perf_counter() - started,
                "data/stage_bytes": downloaded,
                "data/stage_skipped_bytes": skipped,
            },
            0,
        )
    return root

def _active_run_sink() -> _MetricsSink | None:
    try:
        from tau import stellar
    except ImportError:
        return None
    run = stellar.active_run()
    return None if run is None else run.log

def _close(iterator: object) -> None:
    close = getattr(iterator, "close", None)
    if callable(close):
        close()

def _timed(iterator: Iterator[_T], name: str, sink: _MetricsSink, step: int) -> Iterator[_T]:
    wait_key = f"{name}/wait_seconds"
    step_key = f"{name}/step_seconds"
    started = perf_counter()
    previous_requested: float | None = None
    previous_wait = 0.0
    total_wait = 0.0
    try:
        while True:
            requested = perf_counter()
            try:
                item = next(iterator)
            except StopIteration:
                break
            wait = perf_counter() - requested
            if previous_requested is not None:
                sink({wait_key: previous_wait, step_key: requested - previous_requested}, step)
                step += 1
            previous_requested = requested
            previous_wait = wait
            total_wait += wait
            yield item
    except GeneratorExit:
        requested = perf_counter()
        _log_final_step(sink, name, step, previous_requested, previous_wait, total_wait, started, requested)
        raise
    _log_final_step(sink, name, step, previous_requested, previous_wait, total_wait, started, requested)

def _log_final_step(
    sink: _MetricsSink,
    name: str,
    step: int,
    previous_requested: float | None,
    previous_wait: float,
    total_wait: float,
    started: float,
    ended: float,
) -> None:
    if previous_requested is None:
        return
    sink(
        {
            f"{name}/wait_seconds": previous_wait,
            f"{name}/step_seconds": ended - previous_requested,
            f"{name}/epoch_wait_seconds": total_wait,
            f"{name}/epoch_seconds": ended - started,
        },
        step,
    )

class _Cancelled(Exception):
    pass

class _StageFile(NamedTuple):
    uri: str
    sha256: str
    relpath: str
    target: Path
    size: int | None

def _stage_identity(ref: str | None, name: str | None, version: str | None) -> tuple[str | None, str | None]:
    if ref is not None:
        ref_name, separator, ref_version = str(ref).strip().partition("@")
        if not separator or not ref_name or not ref_version:
            raise ValueError(f"dataset ref must be NAME@VERSION: {ref!r}")
        if (name is not None and name != ref_name) or (version is not None and version != ref_version):
            raise ValueError(f"dataset ref {ref!r} conflicts with name={name!r} version={version!r}")
        name, version = ref_name, ref_version
    for field_name, value in (("name", name), ("version", version)):
        if value is not None and (value in ("", ".", "..") or "/" in value or "\\" in value):
            raise ValueError(f"dataset {field_name} must be a single path component: {value!r}")
    return name, version

def _stage_sources(
    env_prefix: str | None,
    uris: Sequence[str] | None,
    sha256s: Sequence[str] | None,
) -> tuple[list[str], list[str]]:
    if uris is not None:
        if env_prefix is not None:
            raise ValueError("pass either uris= or env_prefix=, not both")
        if sha256s is None:
            raise ValueError("sha256s= is required with uris=")
        uri_label, sha_label = "uris", "sha256s"
        uri_list = [str(uri).strip() for uri in uris]
        sha_list = [str(sha).strip() for sha in sha256s]
    else:
        if sha256s is not None:
            raise ValueError("sha256s= requires uris=")
        prefix = env_prefix or _DEFAULT_ENV_PREFIX
        uri_label, sha_label = f"{prefix}_URIS", f"{prefix}_SHA256S"
        uri_list = _csv_env(uri_label)
        sha_list = _csv_env(sha_label)
    if not uri_list:
        raise ValueError(f"{uri_label} lists no files to stage")
    if not all(uri_list):
        raise ValueError(f"{uri_label} contains an empty entry")
    if len(sha_list) != len(uri_list):
        raise ValueError(f"{sha_label} has {len(sha_list)} entries but {uri_label} has {len(uri_list)}")
    return uri_list, [_normalize_sha256(sha, sha_label) for sha in sha_list]

def _csv_env(name: str) -> list[str]:
    return [item.strip() for item in os.environ.get(name, "").split(",") if item.strip()]

def _normalize_sha256(value: str, label: str) -> str:
    digest = value.lower().removeprefix("sha256:")
    if len(digest) != 64 or any(char not in "0123456789abcdef" for char in digest):
        raise ValueError(f"{label} entry is not a SHA-256 hex digest: {value!r}")
    return digest

def _stage_sizes(sizes: Sequence[int] | None, count: int) -> list[int | None]:
    if sizes is None:
        return [None] * count
    size_list: list[int | None] = [int(size) for size in sizes]
    if len(size_list) != count:
        raise ValueError(f"sizes has {len(size_list)} entries but there are {count} files")
    if any(size is not None and size < 0 for size in size_list):
        raise ValueError("sizes must be non-negative")
    return size_list

def _stage_dest(name: str | None, version: str | None) -> Path:
    if not name or not version:
        raise ValueError("stage needs ref='NAME@VERSION', name= and version=, or an explicit dest=")
    return _datasets_root() / name / version

def _datasets_root() -> Path:
    datasets_dir = os.environ.get("TAU_DATASETS_DIR", "").strip()
    if datasets_dir:
        return Path(datasets_dir)
    hot_dir = os.environ.get("TAU_HOT_DIR", "").strip()
    if hot_dir:
        return Path(hot_dir) / "datasets"
    return Path("/mnt/datasets")

def _stage_plan(root: Path, uris: list[str], sha256s: list[str], sizes: list[int | None]) -> list[_StageFile]:
    keys = [_source_key(uri) for uri in uris]
    common = _common_prefix_length([key[:-1] for key in keys])
    files: list[_StageFile] = []
    seen: set[str] = set()
    for uri, sha256, size, key in zip(uris, sha256s, sizes, keys):
        parts = key[common:]
        relpath = "/".join(parts)
        if relpath in seen:
            raise ValueError(f"more than one source stages to {relpath!r}")
        seen.add(relpath)
        known_size = size if size is not None else _local_size(uri)
        files.append(_StageFile(uri, sha256, relpath, root.joinpath(*parts), known_size))
    return files

def _common_prefix_length(keys: list[tuple[str, ...]]) -> int:
    length = 0
    for parts in zip(*keys):
        if any(part != parts[0] for part in parts):
            break
        length += 1
    return length

def _source_key(uri: str) -> tuple[str, ...]:
    parsed = urllib.parse.urlsplit(uri)
    scheme = parsed.scheme.lower()
    if scheme in ("http", "https"):
        origin = f"{scheme}://{parsed.netloc.lower()}"
        segments = urllib.parse.unquote(parsed.path).split("/")
    elif scheme in ("", "file"):
        origin = "file://"
        segments = _local_source(uri).replace("\\", "/").split("/")
    else:
        raise ValueError(f"unsupported dataset source {_safe_uri(uri)}: use file://, https://, or a local path")
    segments = [segment for segment in segments if segment not in ("", ".")]
    if not segments or ".." in segments:
        raise ValueError(f"dataset source must name a file without '..': {_safe_uri(uri)}")
    return (origin, *segments)

def _local_source(uri: str) -> str:
    parsed = urllib.parse.urlsplit(uri)
    if parsed.scheme.lower() != "file":
        return uri
    if parsed.netloc not in ("", "localhost"):
        raise ValueError(f"file URI must be absolute (file:///path): {uri}")
    return urllib.request.url2pathname(parsed.path)

def _local_size(uri: str) -> int | None:
    if urllib.parse.urlsplit(uri).scheme.lower() in ("http", "https"):
        return None
    try:
        return os.stat(_local_source(uri)).st_size
    except OSError:
        return None

def _safe_uri(uri: str) -> str:
    parsed = urllib.parse.urlsplit(uri)
    netloc = parsed.netloc.rpartition("@")[2]
    return urllib.parse.urlunsplit((parsed.scheme, netloc, parsed.path, "", ""))

def _manifest_digest(files: list[_StageFile]) -> str:
    entries = sorted([file.relpath, file.sha256] for file in files)
    return hashlib.sha256(json.dumps(entries, separators=(",", ":")).encode("utf-8")).hexdigest()

def _stage_per_node(
    root: Path, files: list[_StageFile], label: str, max_workers: int, retries: int, timeout: float
) -> tuple[int, int]:
    marker = root / _STAGE_MARKER
    manifest = _manifest_digest(files)
    with _stage_lock(root / _STAGE_LOCK, label, timeout):
        present = _completed_size(marker, manifest, files)
        if present is not None:
            return 0, present
        marker.unlink(missing_ok=True)
        for partial in root.rglob(f"*{_PARTIAL_SUFFIX}"):
            partial.unlink(missing_ok=True)
        staged = _stage_all(root, files, label, max_workers, retries)
        _write_marker(marker, manifest, files)
        return staged

@contextlib.contextmanager
def _stage_lock(path: Path, label: str, timeout: float) -> Iterator[None]:
    deadline = monotonic() + timeout
    with path.open("a") as handle:
        while True:
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if monotonic() >= deadline:
                    raise StageError(
                        f"timed out after {timeout:g}s waiting for another process to stage {label} into {path.parent}"
                    ) from None
                sleep(_POLL_SECONDS)
        yield

def _file_stamp(path: Path) -> list[int]:
    info = path.stat()
    return [info.st_size, info.st_mtime_ns, info.st_ino]

def _completed_size(marker: Path, manifest: str, files: list[_StageFile]) -> int | None:
    try:
        record = json.loads(marker.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if not isinstance(record, dict) or record.get("manifest") != manifest or not isinstance(record.get("files"), dict):
        return None
    total = 0
    for file in files:
        try:
            stamp = _file_stamp(file.target)
        except OSError:
            return None
        if record["files"].get(file.relpath) != stamp:
            return None
        total += stamp[0]
    return total

def _write_marker(marker: Path, manifest: str, files: list[_StageFile]) -> None:
    record = {"manifest": manifest, "files": {file.relpath: _file_stamp(file.target) for file in files}}
    pending = marker.with_name(f"{marker.name}.{uuid.uuid4().hex}.tmp")
    try:
        pending.write_text(json.dumps(record), encoding="utf-8")
        os.replace(pending, marker)
    finally:
        pending.unlink(missing_ok=True)

def _stage_all(root: Path, files: list[_StageFile], label: str, max_workers: int, retries: int) -> tuple[int, int]:
    stop = threading.Event()
    with ThreadPoolExecutor(max_workers=min(max_workers, len(files)), thread_name_prefix="tau-stage") as pool:
        try:
            present = _run_all(pool, stop, [lambda file=file: _staged_size(file, stop) for file in files])
            pending = [file for file, size in zip(files, present) if size is None]
            _check_space(root, sum(file.size or 0 for file in pending), label)
            downloaded = _run_all(pool, stop, [lambda file=file: _fetch(file, label, retries, stop) for file in pending])
        except BaseException:
            stop.set()
            raise
    return sum(downloaded), sum(size for size in present if size is not None)

def _run_all(pool: ThreadPoolExecutor, stop: threading.Event, calls: list[Callable[[], _T]]) -> list[_T]:
    futures = [pool.submit(call) for call in calls]
    done, _ = wait(futures, return_when=FIRST_EXCEPTION)
    failure = _first_failure(futures, done)
    if failure is not None:
        stop.set()
        for future in futures:
            future.cancel()
        raise failure
    return [future.result() for future in futures]

def _first_failure(futures: list[Future[_T]], done: set[Future[_T]]) -> BaseException | None:
    errors = [future.exception() for future in futures if future in done and not future.cancelled()]
    failures = [error for error in errors if error is not None]
    real = [error for error in failures if not isinstance(error, _Cancelled)]
    return (real or failures or [None])[0]

def _staged_size(file: _StageFile, stop: threading.Event) -> int | None:
    if not file.target.is_file():
        return None
    size = file.target.stat().st_size
    if file.size is not None and size != file.size:
        return None
    digest = hashlib.sha256()
    with file.target.open("rb") as handle:
        while chunk := handle.read(_CHUNK_BYTES):
            if stop.is_set():
                raise _Cancelled
            digest.update(chunk)
    return size if digest.hexdigest() == file.sha256 else None

def _check_space(directory: Path, needed: int, what: str) -> None:
    if needed <= 0:
        return
    free = shutil.disk_usage(directory).free
    if needed > free:
        raise StageError(
            f"not enough space to stage {what}: {needed} bytes needed, {free} free under {directory}; "
            "raise storage.hot_size in tau.yaml or free space on the hot volume"
        )

def _fetch(file: _StageFile, label: str, retries: int, stop: threading.Event) -> int:
    attempt = 0
    while True:
        try:
            return _download(file, stop)
        except (StageError, _Cancelled):
            raise
        except Exception as exc:
            if isinstance(exc, urllib.error.HTTPError):
                exc.close()
            if attempt >= retries or not _is_transient(exc):
                raise StageError(
                    f"failed to stage {file.relpath} of {label} from {_safe_uri(file.uri)} "
                    f"after {attempt + 1} attempt(s): {exc}"
                ) from exc
            attempt += 1
            if stop.wait(_RETRY_BACKOFF_SECONDS * 2 ** (attempt - 1) * (1 + random.random())):
                raise _Cancelled from exc

def _is_transient(exc: BaseException) -> bool:
    if isinstance(exc, urllib.error.HTTPError):
        return exc.code in (408, 425, 429) or exc.code >= 500
    return isinstance(exc, (ConnectionError, TimeoutError, http.client.HTTPException, urllib.error.URLError))

def _download(file: _StageFile, stop: threading.Event) -> int:
    file.target.parent.mkdir(parents=True, exist_ok=True)
    partial = file.target.with_name(f".{file.target.name}.{uuid.uuid4().hex}{_PARTIAL_SUFFIX}")
    digest = hashlib.sha256()
    written = 0
    try:
        with _open_source(file.uri) as source, partial.open("wb") as out:
            length = _content_length(source)
            if file.size is None and length is not None:
                _check_space(file.target.parent, length, file.relpath)
            while chunk := source.read(_CHUNK_BYTES):
                if stop.is_set():
                    raise _Cancelled
                digest.update(chunk)
                out.write(chunk)
                written += len(chunk)
            if length is not None and written != length:
                raise ConnectionError(f"response ended after {written} of {length} bytes")
        actual = digest.hexdigest()
        if actual != file.sha256:
            raise StageError(
                f"sha256 mismatch for {file.relpath} from {_safe_uri(file.uri)}: "
                f"expected {file.sha256}, got {actual}"
            )
        os.replace(partial, file.target)
    except OSError as exc:
        if exc.errno in _SPACE_ERRNOS:
            raise StageError(
                f"no space left writing {file.relpath} under {file.target.parent} ({os.strerror(exc.errno)}); "
                "raise storage.hot_size in tau.yaml or free space on the hot volume"
            ) from exc
        raise
    finally:
        partial.unlink(missing_ok=True)
    return written

def _open_source(uri: str) -> BinaryIO:
    if urllib.parse.urlsplit(uri).scheme.lower() in ("http", "https"):
        return urllib.request.urlopen(uri, timeout=_REQUEST_TIMEOUT_SECONDS)
    return open(_local_source(uri), "rb")

def _content_length(source: object) -> int | None:
    headers = getattr(source, "headers", None)
    value = headers.get("Content-Length") if headers is not None else None
    return int(value) if value is not None and str(value).isdigit() else None

__all__ = ["StageError", "TimedIterator", "dataset_file_reference", "stage", "timed"]
