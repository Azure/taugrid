# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""W&B-style experiment logging for Tau/Stellar.

The logger keeps the integration point familiar: training code calls
``stellar.log({"train/loss": loss}, step=step)`` and records media with
``stellar.Image(...)`` / ``stellar.Html(...)``. Local files are written first so
cluster jobs can run offline, then ``finish(sync=True)`` shells to the
``taugrid-portal`` CLI to import metrics and artifact pointers into the
experiment store. Pass ``tau_binary=`` or set ``TAUGRID_PORTAL_BINARY`` to point
at a specific binary.
"""

from __future__ import annotations

import hashlib
import importlib.metadata
import json
import math
import os
import platform
import re
import shutil
import sqlite3
import subprocess
import sys
import time
import warnings
from argparse import Namespace
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping

try:  # Lightning stays optional for lightweight Tau SDK installs.
    from lightning.pytorch.loggers import Logger as _LightningLogger
except ModuleNotFoundError:  # pragma: no cover - exercised when Lightning is absent.
    _LightningLogger = object

_ACTIVE_RUN: "Run | None" = None


@dataclass(frozen=True)
class Image:
    """Image artifact logged to Stellar."""

    path: str | os.PathLike[str]
    caption: str | None = None


@dataclass(frozen=True)
class Html:
    """HTML artifact logged to Stellar.

    ``content`` may be raw HTML or a path to an existing ``.html`` file.
    """

    content: str | os.PathLike[str]
    caption: str | None = None


@dataclass(frozen=True)
class Table:
    """JSON table artifact logged to Stellar."""

    rows: Iterable[Mapping[str, Any]]
    columns: Iterable[str] | None = None
    caption: str | None = None


@dataclass
class _Artifact:
    key: str
    kind: str
    path: Path
    name: str
    caption: str | None = None
    step: int | None = None
    direction: str = "output"
    alias: str | None = None
    external_ref: str | None = None
    source_artifact_id: str | None = None
    source_run_id: str | None = None
    source_dataset_name: str | None = None
    source_dataset_version: str | None = None
    source_dataset_digest: str | None = None


class Run:
    """A W&B-shaped run handle backed by Tau/Stellar files."""

    def __init__(
        self,
        *,
        project: str,
        name: str,
        group: str | None = None,
        experiment: str | None = None,
        experiment_id: str | None = None,
        config: Mapping[str, Any] | None = None,
        dir: str | os.PathLike[str] | None = None,
        store: str | os.PathLike[str] | None = None,
        owner: str | None = None,
        tau_binary: str | None = None,
    ):
        self.project = _required("project", project)
        self.name = _required("name", name)
        # Smart group default: TAU_GROUP env > JOB_NAME prefix > "default"
        if group is None:
            job_prefix = os.environ.get("JOB_NAME", "").split("-")[0] if os.environ.get("JOB_NAME") else None
            group = os.environ.get("TAU_GROUP") or job_prefix or "default"
        self.group = group or "default"
        # Stellar's hierarchy is project -> experiment -> run.
        self.experiment = experiment
        self.experiment_id = (
            experiment_id
            or os.environ.get("TAU_EXPERIMENT_ID")
            or os.environ.get("TAU_EXPERIMENT")
            # Prefer the experiment name over the project: passing
            # experiment="lr-sweep" alone should create an experiment called
            # lr-sweep rather than collapsing every run into one experiment
            # named after the project.
            or (_safe_name(self.experiment) if self.experiment else None)
            or project
        )
        self.store = Path(store).expanduser() if store else None
        self.owner = owner
        # Resolved lazily in sync(): constructing a Run must not require the
        # CLI to be installed, since local logging works entirely offline.
        self.tau_binary = tau_binary
        self.run_dir_name = _safe_name(self.name)
        base = Path(dir or os.environ.get("TAU_STELLAR_DIR", ".tau/stellar")).expanduser()
        self.dir = base / self.run_dir_name
        self.artifacts_dir = self.dir / "artifacts"
        self.history_path = self.dir / "history.jsonl"
        self.config_path = self.dir / "config.json"
        self.manifest_path = self.dir / "run.json"
        self._artifacts: list[_Artifact] = []
        self._closed = False
        self.runtime = _runtime_metadata()
        self.dependencies = _dependency_metadata()
        self.log_uri = str(self.history_path)
        self.dir.mkdir(parents=True, exist_ok=True)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self.config: dict[str, Any] = {}
        if config is not None:
            self.log_config(config)
        self._write_manifest()

    def log(self, data: Mapping[str, Any], step: int | None = None) -> None:
        """Append scalar metrics and media artifacts for one training step."""

        self._ensure_open()
        row: dict[str, Any] = {"_timestamp": time.time()}
        if step is not None:
            row["_step"] = int(step)
        for key, value in data.items():
            if isinstance(value, (Image, Html, Table)):
                self.log_artifact(key, value, step=step)
            else:
                row[key] = _metric_value(key, value)
        if len(row) > (2 if "_step" in row else 1):
            with self.history_path.open("a", encoding="utf-8") as handle:
                handle.write(_json_line(row))

    def log_artifact(
        self,
        name: str,
        value: str | os.PathLike[str] | Image | Html | Table,
        artifact_type: str | None = None,
        *,
        step: int | None = None,
        direction: str = "output",
        alias: str | None = None,
        external_ref: str | None = None,
    ) -> Path:
        """Record a non-scalar artifact and return its local logged path."""

        self._ensure_open()
        kind = artifact_type or _artifact_kind(name, value)
        safe = _artifact_safe_name(name, step)
        caption = _artifact_caption(value)
        if isinstance(value, Image):
            source = Path(value.path)
            dest = self._copy_artifact(source, safe)
        elif isinstance(value, Html):
            dest = self._write_html_artifact(value.content, safe)
        elif isinstance(value, Table):
            dest = self.artifacts_dir / f"{safe}.json"
            rows = [dict(row) for row in value.rows]
            payload: dict[str, Any] = {
                "columns": list(value.columns) if value.columns else list(rows[0].keys()) if rows else [],
                "rows": rows,
            }
            if value.caption:
                payload["caption"] = value.caption
            if step is not None:
                payload["step"] = int(step)
            _write_json(dest, payload)
        else:
            source = Path(value)
            dest = self._copy_artifact(source, safe)
        artifact = _Artifact(
            key=name,
            kind=kind,
            path=dest,
            name=safe,
            caption=caption,
            step=int(step) if step is not None else None,
            direction=_artifact_direction(direction),
            alias=_optional_text(alias),
            external_ref=_optional_text(external_ref),
        )
        self._artifacts = [item for item in self._artifacts if not (item.key == name and item.step == artifact.step)]
        self._artifacts.append(artifact)
        self._write_manifest()
        return dest

    def use_artifact(self, ref: str, *, name: str | None = None) -> Path:
        """Resolve a prior expstore artifact by alias, id, or name and copy it locally."""

        self._ensure_open()
        if self.store is None:
            raise ValueError("stellar Run.use_artifact requires store=...")
        record = _resolve_store_artifact(self.store, ref)
        source = self.store / record["uri"]
        if not source.exists() or not source.is_file():
            raise FileNotFoundError(source)
        artifact_name = name or record["name"]
        safe = _safe_name("input-" + artifact_name)
        dest = self._copy_artifact(source, safe)
        artifact = _Artifact(
            key=artifact_name,
            kind=record["type"],
            path=dest,
            name=safe,
            direction="input",
            alias=record.get("alias") or ref,
            source_artifact_id=record["artifact_id"],
            source_run_id=record["run_id"],
            source_dataset_name=record.get("source_dataset_name") or None,
            source_dataset_version=record.get("source_dataset_version") or None,
            source_dataset_digest=record.get("source_dataset_digest") or None,
        )
        self._artifacts.append(artifact)
        self._write_manifest()
        return dest

    def log_config(self, config: Mapping[str, Any] | Namespace) -> None:
        """Merge run configuration into ``config.json``."""

        self._ensure_open()
        self.config.update(_config_mapping(config))
        _write_json(self.config_path, self.config)

    def finish(self, *, sync: bool = False) -> None:
        """Close the run, optionally syncing local files into a Tau expstore."""

        if self._closed:
            return
        self._write_manifest()
        if sync:
            self.sync()
        self._closed = True

    def __enter__(self) -> "Run":
        self._ensure_open()
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.finish(sync=exc_type is None and self.store is not None)

    def sync(self) -> None:
        """Import this run into a Tau expstore using the ``taugrid-portal`` CLI."""

        if self.store is None:
            raise ValueError("stellar Run.sync requires store=...")
        self.store.mkdir(parents=True, exist_ok=True)
        if self.experiment_id:
            # Also gated on the id, not the display name: `init` bootstraps the
            # store, so skipping it leaves `track` with no manifest.json at all.
            init_args = [
                "experiment", "--store", str(self.store), "init", self.experiment_id,
                "--project", self.project,
            ]
            if self.experiment:
                init_args.extend(["--description", self.experiment])
            self._run_cli(init_args + ["--group", self.group])
        store_artifact_dir = self.store / "artifacts" / self.run_dir_name
        store_artifact_dir.mkdir(parents=True, exist_ok=True)
        artifact_args: list[str] = []
        for artifact in self._artifacts:
            dest = store_artifact_dir / artifact.path.name
            shutil.copy2(artifact.path, dest)
            uri = f"artifacts/{self.run_dir_name}/{dest.name}"
            artifact_args.extend(["--artifact", _artifact_cli_spec(artifact, uri)])
        track_args = [
            "experiment", "--store", str(self.store), "track", self.name,
            "--project", self.project,
            "--group", self.group,
            "--state", "succeeded",
            "--idempotency-key", _sync_idempotency_key("artifacts", self.run_dir_name, self._artifact_fingerprints()),
        ]
        if self.owner:
            track_args.extend(["--owner", self.owner])
        if self.config_path.exists():
            track_args.extend(["--config", str(self.config_path)])
        track_args.extend(["--runtime", self.runtime, "--dependencies", self.dependencies, "--log-uri", self.log_uri])
        self._run_cli(track_args + artifact_args)
        if self.experiment_id:
            # `track` carries no experiment flag, so the link is made here.
            # Gate on the id, not the optional display name: experiment_id is
            # independently settable (argument or TAU_EXPERIMENT_ID), and a run
            # with no history import never reaches the `import jsonl` call
            # below that would otherwise carry it.
            self._run_cli([
                "experiment", "--store", str(self.store), "experiments", "tag-run",
                self.name,
                "--experiment", self.experiment_id,
                "--name", self.experiment or self.experiment_id,
            ])
        if self.history_path.exists() and self.history_path.stat().st_size > 0:
            self._run_cli([
                "experiment", "--store", str(self.store), "import", "jsonl",
                "--run", self.name,
                "--project", self.project,
                "--experiment", self.experiment_id,
                "--group", self.group,
                "--history", str(self.history_path),
                "--source", "stellar-python",
                "--idempotency-key", _sync_idempotency_key("history", self.run_dir_name, [_path_fingerprint(self.history_path)]),
            ])

    def _artifact_fingerprints(self) -> list[str]:
        parts: list[str] = [self.runtime, self.dependencies, self.log_uri]
        if self.config_path.exists():
            parts.append(_path_fingerprint(self.config_path))
        for artifact in self._artifacts:
            parts.extend([
                artifact.key,
                artifact.kind,
                artifact.name,
                artifact.caption or "",
                str(artifact.step) if artifact.step is not None else "",
                artifact.direction,
                artifact.alias or "",
                artifact.external_ref or "",
                artifact.source_artifact_id or "",
                artifact.source_run_id or "",
                artifact.source_dataset_name or "",
                artifact.source_dataset_version or "",
                artifact.source_dataset_digest or "",
                _path_fingerprint(artifact.path),
            ])
        return parts

    def _copy_artifact(self, source: Path, safe: str) -> Path:
        if not source.exists() or not source.is_file():
            raise FileNotFoundError(source)
        suffix = source.suffix or ".artifact"
        dest = self.artifacts_dir / f"{safe}{suffix}"
        shutil.copy2(source, dest)
        return dest

    def _write_html_artifact(self, content: str | os.PathLike[str], safe: str) -> Path:
        source = Path(content)
        if source.exists() and source.is_file():
            return self._copy_artifact(source, safe)
        dest = self.artifacts_dir / f"{safe}.html"
        dest.write_text(str(content), encoding="utf-8")
        return dest

    def _portal_binary(self) -> str:
        if self.tau_binary:
            return self.tau_binary
        from tau.workloads import _find_portal_binary

        self.tau_binary = _find_portal_binary()
        return self.tau_binary

    def _run_cli(self, args: list[str]) -> None:
        subprocess.run([self._portal_binary(), *args], check=True)

    def _write_manifest(self) -> None:
        payload = {
            "project": self.project,
            "name": self.name,
            "group": self.group,
            "experiment": self.experiment,
            "experiment_id": self.experiment_id,
            "history": str(self.history_path),
            "artifacts": [
                {"key": item.key, "type": item.kind, "path": str(item.path), "name": item.name}
                | ({"caption": item.caption} if item.caption else {})
                | ({"step": item.step} if item.step is not None else {})
                | ({"direction": item.direction} if item.direction else {})
                | ({"alias": item.alias} if item.alias else {})
                | ({"source_artifact_id": item.source_artifact_id} if item.source_artifact_id else {})
                | ({"source_run_id": item.source_run_id} if item.source_run_id else {})
                for item in self._artifacts
            ],
        }
        _write_json(self.manifest_path, payload)

    def _ensure_open(self) -> None:
        if self._closed:
            raise RuntimeError("stellar run is already finished")


class StellarLogger(_LightningLogger):
    """PyTorch Lightning logger that writes to a local Stellar run packet."""

    def __init__(
        self,
        *,
        project: str,
        name: str | None = None,
        run: str | None = None,
        group: str | None = None,
        experiment: str | None = None,
        experiment_id: str | None = None,
        config: Mapping[str, Any] | Namespace | None = None,
        dir: str | os.PathLike[str] | None = None,
        store: str | os.PathLike[str] | None = None,
        owner: str | None = None,
        tau_binary: str | None = None,
        sync_on_finalize: bool | None = None,
    ):
        super().__init__()
        run_name = name or run
        if not run_name:
            raise ValueError("StellarLogger name=... is required")
        self._run = Run(
            project=project,
            name=run_name,
            group=group,
            experiment=experiment,
            experiment_id=experiment_id,
            config=_config_mapping(config) if config is not None else None,
            dir=dir,
            store=store,
            owner=owner,
            tau_binary=tau_binary,
        )
        self._save_dir = str(Path(dir or os.environ.get("TAU_STELLAR_DIR", ".tau/stellar")).expanduser())
        self._sync_on_finalize = self._run.store is not None if sync_on_finalize is None else sync_on_finalize
        self._warned_metric_keys: set[str] = set()

    @property
    def experiment(self) -> Run:
        return self._run

    @property
    def name(self) -> str:
        return self._run.name

    @property
    def version(self) -> str:
        return self._run.run_dir_name

    @property
    def save_dir(self) -> str:
        return self._save_dir

    def log_metrics(self, metrics: Mapping[str, Any], step: int | None = None) -> None:
        row: dict[str, float] = {}
        for key, value in metrics.items():
            metric = _logger_metric_value(value)
            if metric is None:
                self._warn_once(key, f"StellarLogger skipped unsupported or non-finite metric {key!r}")
                continue
            row[key] = metric
        if row:
            self._run.log(row, step=step)

    def log_hyperparams(self, params: Mapping[str, Any] | Namespace, *args: Any, **kwargs: Any) -> None:
        if args:
            raise TypeError("StellarLogger.log_hyperparams does not accept positional args after params")
        config = _config_mapping(params)
        if kwargs:
            config.update(_config_mapping(kwargs))
        self._run.log_config(config)

    def save(self) -> None:
        self._run._write_manifest()

    def finalize(self, status: str) -> None:
        successful = str(status).lower() in {"", "success", "succeeded", "finished", "completed"}
        self._run.finish(sync=self._sync_on_finalize and successful)

    def after_save_checkpoint(self, checkpoint_callback: Any) -> None:
        paths = _checkpoint_paths(checkpoint_callback)
        for label, path in paths:
            if not path.exists():
                warnings.warn(f"StellarLogger checkpoint artifact {path} does not exist", RuntimeWarning, stacklevel=2)
                continue
            try:
                self._run.log_artifact(f"checkpoint/{label}", path, artifact_type="checkpoint")
            except (FileNotFoundError, OSError) as exc:
                warnings.warn(f"StellarLogger could not log checkpoint {path}: {exc}", RuntimeWarning, stacklevel=2)

    def _warn_once(self, key: str, message: str) -> None:
        if key in self._warned_metric_keys:
            return
        self._warned_metric_keys.add(key)
        warnings.warn(message, RuntimeWarning, stacklevel=3)


def init(**kwargs: Any) -> Run:
    """Create and set the active Stellar run."""

    global _ACTIVE_RUN
    if "run" in kwargs and "name" not in kwargs:
        kwargs["name"] = kwargs.pop("run")
    _ACTIVE_RUN = Run(**kwargs)
    return _ACTIVE_RUN


def log(data: Mapping[str, Any], step: int | None = None) -> None:
    _current().log(data, step=step)


def log_artifact(
    name: str,
    value: str | os.PathLike[str] | Image | Html | Table,
    artifact_type: str | None = None,
    *,
    step: int | None = None,
    direction: str = "output",
    alias: str | None = None,
    external_ref: str | None = None,
) -> Path:
    return _current().log_artifact(name, value, artifact_type=artifact_type, step=step, direction=direction, alias=alias, external_ref=external_ref)


def use_artifact(ref: str, *, name: str | None = None) -> Path:
    return _current().use_artifact(ref, name=name)


def finish(*, sync: bool = False) -> None:
    global _ACTIVE_RUN
    _current().finish(sync=sync)
    _ACTIVE_RUN = None


def _current() -> Run:
    if _ACTIVE_RUN is None:
        raise RuntimeError("call tau.stellar.init(...) before logging")
    return _ACTIVE_RUN


def _required(name: str, value: str | None) -> str:
    if not value:
        raise ValueError(f"stellar {name} is required")
    return value


def _write_json(path: Path, payload: Any) -> None:
    path.write_text(json.dumps(payload, indent=2, sort_keys=True, allow_nan=False) + "\n", encoding="utf-8")


def _json_line(payload: Any) -> str:
    return json.dumps(payload, sort_keys=True, allow_nan=False) + "\n"


def _metric_value(key: str, value: Any) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TypeError(
            "stellar.log only accepts numeric scalars or Image/Html/Table "
            f"values; {key!r} has {type(value).__name__}"
        )
    metric = float(value)
    if not math.isfinite(metric):
        raise ValueError(f"stellar.log metric {key!r} must be finite")
    return metric


def _safe_name(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "-", value.strip()).strip("-") or "artifact"


def _artifact_kind(name: str, value: Any) -> str:
    if isinstance(value, Table):
        return "table"
    if isinstance(value, Html):
        return "report"
    if isinstance(value, Image):
        haystack = f"{name} {value}".lower()
        if re.search(r"retrieval|nearest|neighbor", haystack):
            return "retrieval"
        if re.search(r"embedding|projection", haystack):
            return "embedding"
        return "image"
    haystack = f"{name} {value}".lower()
    if re.search(r"image|overlay|grounding|attention|retrieval|embedding|projection|png|jpe?g|gif|webp|svg", haystack):
        if re.search(r"retrieval|nearest|neighbor", haystack):
            return "retrieval"
        if re.search(r"embedding|projection", haystack):
            return "embedding"
        return "image"
    if re.search(r"html|report|diff|caption|vqa", haystack):
        return "report"
    if re.search(r"table|json|examples", haystack):
        return "table"
    if re.search(r"checkpoint|model|safetensors|pt|pth|ckpt", haystack):
        return "checkpoint"
    return "artifact"


def _artifact_safe_name(name: str, step: int | None) -> str:
    safe = _safe_name(name)
    if step is None:
        return safe
    return f"{safe}-step-{int(step)}"


def _artifact_caption(value: Any) -> str | None:
    caption = getattr(value, "caption", None)
    if caption is None:
        return None
    caption = str(caption).strip()
    return caption or None


def _artifact_cli_spec(artifact: _Artifact, uri: str) -> str:
    payload = {
        "type": artifact.kind,
        "name": artifact.name,
        "uri": uri,
        "direction": artifact.direction,
    }
    for key, value in {
        "caption": artifact.caption,
        "alias": artifact.alias,
        "external_ref": artifact.external_ref,
        "source_artifact_id": artifact.source_artifact_id,
        "source_run_id": artifact.source_run_id,
        "source_dataset_name": artifact.source_dataset_name,
        "source_dataset_version": artifact.source_dataset_version,
        "source_dataset_digest": artifact.source_dataset_digest,
    }.items():
        if value:
            payload[key] = value
    return json.dumps(payload, sort_keys=True, separators=(",", ":"))


def _sync_idempotency_key(kind: str, run_dir_name: str, parts: Iterable[str]) -> str:
    digest = hashlib.sha256()
    for part in parts:
        digest.update(str(part).encode("utf-8"))
        digest.update(b"\0")
    return f"stellar-{run_dir_name}-{kind}-{digest.hexdigest()[:12]}"


def _path_fingerprint(path: Path) -> str:
    stat = path.stat()
    return f"{path.name}:{stat.st_size}:{stat.st_mtime_ns}"


def _artifact_direction(value: str) -> str:
    direction = (value or "output").strip().lower()
    if direction not in {"input", "output"}:
        raise ValueError("stellar artifact direction must be 'input' or 'output'")
    return direction


def _optional_text(value: str | None) -> str | None:
    if value is None:
        return None
    value = str(value).strip()
    return value or None


def _resolve_store_artifact(store: Path, ref: str) -> dict[str, str]:
    ref = ref.strip()
    if not ref:
        raise ValueError("artifact reference is required")
    db_path = store / "index.sqlite"
    if not db_path.exists():
        raise FileNotFoundError(db_path)
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    try:
        row = conn.execute(
            """
SELECT artifact_id, run_id, type, uri, name, coalesce(alias, '') AS alias,
       coalesce(source_dataset_name, '') AS source_dataset_name,
       coalesce(source_dataset_version, '') AS source_dataset_version,
       coalesce(source_dataset_digest, '') AS source_dataset_digest
FROM artifacts
WHERE alias = ? OR artifact_id = ? OR name = ?
ORDER BY created_at DESC, artifact_id DESC
LIMIT 1
""",
            (ref, ref, ref),
        ).fetchone()
    finally:
        conn.close()
    if row is None:
        raise FileNotFoundError(f"artifact {ref!r} was not found in {store}")
    return {key: str(row[key]) for key in row.keys()}


def _runtime_metadata() -> str:
    payload = {
        "python": sys.version.split()[0],
        "implementation": platform.python_implementation(),
        "platform": platform.platform(),
        "executable": sys.executable,
    }
    return json.dumps(payload, sort_keys=True, separators=(",", ":"))


def _dependency_metadata() -> str:
    packages = []
    for dist in sorted(importlib.metadata.distributions(), key=lambda item: item.metadata.get("Name", "").lower()):
        name = dist.metadata.get("Name")
        version = dist.version
        if name and version:
            packages.append({"name": name, "version": version})
        if len(packages) >= 200:
            break
    return json.dumps({"packages": packages}, sort_keys=True, separators=(",", ":"))


def _config_mapping(config: Mapping[str, Any] | Namespace) -> dict[str, Any]:
    if isinstance(config, Namespace):
        raw = vars(config)
    elif isinstance(config, Mapping):
        raw = dict(config)
    else:
        raise TypeError(f"stellar config must be a mapping or argparse.Namespace, got {type(config).__name__}")
    return {str(key): _jsonable(value) for key, value in raw.items()}


def _jsonable(value: Any) -> Any:
    if isinstance(value, (str, int, bool)) or value is None:
        return value
    if isinstance(value, float):
        if not math.isfinite(value):
            return str(value)
        return value
    if isinstance(value, os.PathLike):
        return os.fspath(value)
    if isinstance(value, Mapping):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple, set)):
        return [_jsonable(item) for item in value]
    if isinstance(value, Namespace):
        return _config_mapping(value)
    return str(value)


def _logger_metric_value(value: Any) -> float | None:
    value = _tensor_scalar_value(value)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    metric = float(value)
    if not math.isfinite(metric):
        return None
    return metric


def _tensor_scalar_value(value: Any) -> Any:
    for method in ("detach", "cpu"):
        candidate = getattr(value, method, None)
        if callable(candidate):
            value = candidate()
    item = getattr(value, "item", None)
    if callable(item):
        try:
            return item()
        except (TypeError, ValueError, RuntimeError):
            return value
    return value


def _checkpoint_paths(checkpoint_callback: Any) -> list[tuple[str, Path]]:
    candidates: list[tuple[str, Any]] = [
        ("best", getattr(checkpoint_callback, "best_model_path", "")),
        ("last", getattr(checkpoint_callback, "last_model_path", "")),
    ]
    best_k = getattr(checkpoint_callback, "best_k_models", None)
    if isinstance(best_k, Mapping):
        for idx, path in enumerate(best_k.keys(), start=1):
            candidates.append((f"top-{idx}", path))
    seen: set[Path] = set()
    paths: list[tuple[str, Path]] = []
    for label, raw_path in candidates:
        if not raw_path:
            continue
        path = Path(raw_path)
        if path in seen:
            continue
        seen.add(path)
        paths.append((label, path))
    return paths


__all__ = ["Html", "Image", "Run", "StellarLogger", "Table", "finish", "init", "log", "log_artifact", "use_artifact"]
