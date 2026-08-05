"""Deterministic build artifacts for decorated Tau train/eval workflows."""

from __future__ import annotations

import hashlib
import os
import pathlib
import re
import shutil
import tempfile
from contextlib import contextmanager
from dataclasses import dataclass
from typing import Any, Iterator, Mapping, Optional, Sequence

import yaml

from tau._artifacts import DEFAULT_CHECKPOINT_ARTIFACT, validate_checkpoint_artifact
from tau._cluster import render_wrapper
from tau.config import SecretSource
from tau.workloads import (
    DEFAULT_WORKLOAD_KIND,
    EVAL_WORKLOAD_KIND,
    _build_secret_payload,
    _generated_run_config,
    _prepare_secret_references,
    _resolve_secret_values,
    _write_job_secret_file,
)

BUILD_KIND = "tau.python.build"
BUILD_SCHEMA_VERSION = 1
BUILD_MANIFEST = "tau-build.yaml"
GENERATOR_NAME = "tau-py"

_WORKLOAD_NAME_RE = re.compile(r"^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$")
_RESERVED_BUILD_FILENAMES = frozenset({"tau_py_wrapper.py", "tau.yaml"})


class BuildArtifactError(RuntimeError):
    """A deterministic Tau build could not be created or replayed."""


@dataclass(frozen=True)
class BuildOverrides:
    namespace: Optional[str] = None
    data_pvc: Optional[str] = None
    queue: Optional[str] = None
    gpu_class: Optional[str] = None
    node_selector: Any = None
    disable_default_priorities: bool = False
    cpu_request: Optional[int] = None
    memory_request: Optional[str] = None
    cpu_limit: Optional[int] = None
    memory_limit: Optional[str] = None
    worker_cpu_request: Optional[int] = None
    worker_memory_request: Optional[str] = None
    worker_cpu_limit: Optional[int] = None
    worker_memory_limit: Optional[str] = None
    profiler: Optional[str] = None
    profile_rank: Optional[str] = None
    profile_warmup: Optional[str] = None
    profile_duration: Optional[str] = None
    upstream_checkpoint: Optional[str] = None

    def resource_values(self) -> dict[str, object]:
        return {
            "cpu_request": self.cpu_request,
            "memory_request": self.memory_request,
            "cpu_limit": self.cpu_limit,
            "memory_limit": self.memory_limit,
            "worker_cpu_request": self.worker_cpu_request,
            "worker_memory_request": self.worker_memory_request,
            "worker_cpu_limit": self.worker_cpu_limit,
            "worker_memory_limit": self.worker_memory_limit,
        }


def build_artifact(
    *,
    train_handles: Sequence[tuple[str, Any]],
    eval_handles: Sequence[tuple[str, Any]],
    serve_handles: Sequence[tuple[str, Any]],
    output_dir: pathlib.Path,
    generator_version: str,
    overrides: BuildOverrides,
    force: bool = False,
) -> pathlib.Path:
    """Write a canonical generated artifact and atomically publish its directory."""

    _validate_handle_counts(train_handles, eval_handles)
    if not train_handles and not eval_handles:
        if serve_handles:
            raise BuildArtifactError(
                "tau python build: tau.serve handles remain separate from managed "
                "train/eval artifacts; use ServeHandle.deploy() or tau serve deploy"
            )
        raise BuildArtifactError("tau python build: no @tau.train or @tau.eval handles found")

    train_handle = train_handles[0][1] if train_handles else None
    eval_handle = eval_handles[0][1] if eval_handles else None
    upstream = _resolve_build_upstream(train_handle, eval_handle, overrides.upstream_checkpoint)

    output_dir = output_dir.expanduser()
    parent = output_dir.parent
    parent.mkdir(parents=True, exist_ok=True)
    _check_output_destination(output_dir, force=force)

    tmp_dir = pathlib.Path(tempfile.mkdtemp(prefix=".tau-build-", dir=parent))
    try:
        records: list[dict[str, Any]] = []
        if train_handle is not None:
            records.append(
                _write_workload(
                    tmp_dir,
                    kind="train",
                    attribute=train_handles[0][0],
                    handle=train_handle,
                    overrides=overrides,
                    upstream_checkpoint=None,
                    after=None,
                )
            )
        if eval_handle is not None:
            records.append(
                _write_workload(
                    tmp_dir,
                    kind="eval",
                    attribute=eval_handles[0][0],
                    handle=eval_handle,
                    overrides=overrides,
                    upstream_checkpoint=upstream["checkpoint"],
                    after=upstream.get("after"),
                )
            )

        index: dict[str, Any] = {
            "generator": {
                "name": GENERATOR_NAME,
                "version": generator_version,
            },
            "kind": BUILD_KIND,
            "schema_version": BUILD_SCHEMA_VERSION,
            "workloads": records,
        }
        if serve_handles:
            index["separate_serve"] = [
                {
                    "attribute": attribute,
                    "name": str(handle.name),
                    "reason": "tau.serve maps directly to tau serve deploy and is not a managed workflow",
                }
                for attribute, handle in sorted(serve_handles, key=lambda item: (item[0], item[1].name))
            ]
        _write_canonical_yaml(tmp_dir / BUILD_MANIFEST, index)

        if output_dir.exists() or output_dir.is_symlink():
            shutil.rmtree(output_dir)
        os.replace(tmp_dir, output_dir)
        return output_dir
    finally:
        if tmp_dir.exists():
            shutil.rmtree(tmp_dir)


def load_artifact(path: pathlib.Path) -> tuple[pathlib.Path, dict[str, Any]]:
    """Load and verify a build index, staged-file paths, sizes, and digests."""

    root = path.expanduser()
    if root.is_file() and root.name == BUILD_MANIFEST:
        root = root.parent
    manifest_path = root / BUILD_MANIFEST
    if not manifest_path.is_file():
        raise BuildArtifactError(f"Tau build manifest not found: {manifest_path}")

    try:
        index = yaml.safe_load(manifest_path.read_text()) or {}
    except (OSError, yaml.YAMLError) as exc:
        raise BuildArtifactError(f"cannot read Tau build manifest {manifest_path}: {exc}") from exc
    if not isinstance(index, Mapping):
        raise BuildArtifactError(f"{manifest_path} must contain a mapping")
    if index.get("kind") != BUILD_KIND:
        raise BuildArtifactError(
            f"{manifest_path} kind must be {BUILD_KIND!r}, got {index.get('kind')!r}"
        )
    if index.get("schema_version") != BUILD_SCHEMA_VERSION:
        raise BuildArtifactError(
            f"{manifest_path} schema_version must be {BUILD_SCHEMA_VERSION}, "
            f"got {index.get('schema_version')!r}"
        )

    workloads = index.get("workloads")
    if not isinstance(workloads, list) or not workloads:
        raise BuildArtifactError(f"{manifest_path} workloads must be a non-empty list")
    seen_paths: set[str] = set()
    for record in workloads:
        if not isinstance(record, Mapping):
            raise BuildArtifactError(f"{manifest_path} workload entries must be mappings")
        config = _artifact_file(root, record.get("config"), subject="workload config")
        files = record.get("files")
        if not isinstance(files, list) or not files:
            raise BuildArtifactError(f"{manifest_path} workload files must be a non-empty list")
        record_paths: set[pathlib.Path] = set()
        for file_record in files:
            if not isinstance(file_record, Mapping):
                raise BuildArtifactError(f"{manifest_path} file entries must be mappings")
            raw_path = file_record.get("path")
            file_path = _artifact_file(root, raw_path, subject="staged file")
            normalized = str(raw_path)
            if normalized in seen_paths:
                raise BuildArtifactError(f"{manifest_path} repeats staged file path {normalized!r}")
            seen_paths.add(normalized)
            record_paths.add(file_path)
            expected_size = file_record.get("size")
            if not isinstance(expected_size, int) or expected_size < 0:
                raise BuildArtifactError(f"{manifest_path} has invalid size for {normalized!r}")
            actual = file_path.read_bytes()
            if len(actual) != expected_size:
                raise BuildArtifactError(
                    f"Tau build file size mismatch for {normalized}: "
                    f"expected {expected_size}, got {len(actual)}"
                )
            expected_digest = file_record.get("sha256")
            actual_digest = hashlib.sha256(actual).hexdigest()
            if expected_digest != actual_digest:
                raise BuildArtifactError(
                    f"Tau build digest mismatch for {normalized}: "
                    f"expected {expected_digest}, got {actual_digest}"
                )
        if config not in record_paths:
            raise BuildArtifactError(
                f"{manifest_path} workload config {record.get('config')!r} is not listed in files"
            )
    return root.resolve(), dict(index)


@contextmanager
def execution_config(
    root: pathlib.Path,
    workload: Mapping[str, Any],
) -> Iterator[pathlib.Path]:
    """Yield the directly runnable config, materializing only a temporary secret payload."""

    config_path = _artifact_file(root, workload.get("config"), subject="workload config")
    source_records = workload.get("secret_sources") or []
    if not source_records:
        yield config_path
        return
    if not isinstance(source_records, list):
        raise BuildArtifactError("workload secret_sources must be a list")

    sources = _secret_sources_from_records(source_records)
    secret_name = str(workload.get("generated_secret") or "")
    if not secret_name:
        raise BuildArtifactError("workload with secret_sources is missing generated_secret")
    values = _resolve_secret_values(sources)
    payload = _build_secret_payload(secret_name, values.string_data)

    with tempfile.TemporaryDirectory(prefix="tau-build-submit-") as tmp:
        tmp_path = pathlib.Path(tmp)
        payload_path = _write_job_secret_file(tmp_path, payload)
        if payload_path is None:
            raise BuildArtifactError("failed to materialize generated secret payload")
        raw_config = yaml.safe_load(config_path.read_text()) or {}
        if not isinstance(raw_config, dict):
            raise BuildArtifactError(f"{config_path} must contain a mapping")
        _make_local_paths_absolute(raw_config, config_path.parent)
        workflow = raw_config.setdefault("workflow", {})
        if not isinstance(workflow, dict):
            raise BuildArtifactError(f"{config_path} workflow must be a mapping")
        workflow["secret_payload"] = str(payload_path)
        execution_path = tmp_path / config_path.name
        _write_canonical_yaml(execution_path, raw_config)
        yield execution_path


def _write_workload(
    root: pathlib.Path,
    *,
    kind: str,
    attribute: str,
    handle: Any,
    overrides: BuildOverrides,
    upstream_checkpoint: Optional[str],
    after: Optional[str],
) -> dict[str, Any]:
    name = str(handle._params.name)
    if not _WORKLOAD_NAME_RE.fullmatch(name):
        raise BuildArtifactError(
            f"tau python build: workload name {name!r} is not a safe generated path segment"
        )
    rel_dir = pathlib.PurePosixPath("workloads") / f"{kind}-{name}"
    workload_dir = root / pathlib.Path(rel_dir)
    workload_dir.mkdir(parents=True)

    manifest = handle._submission_manifest(
        data_pvc=overrides.data_pvc,
        **overrides.resource_values(),
    )
    manifest, secret_sources, secret_name = _prepare_secret_references(
        manifest,
        getattr(handle, "_env", None),
    )
    extra_scripts = getattr(handle, "_extra_scripts", ())
    for _, destination in extra_scripts:
        if str(destination) in _RESERVED_BUILD_FILENAMES:
            raise BuildArtifactError(
                f"extra script destination {destination!r} collides with a generated build file"
            )

    user_filename = "tau_user_module.py"
    user_path = workload_dir / user_filename
    source_text = getattr(handle, "_source_text", None)
    if source_text is None:
        user_path.write_bytes(pathlib.Path(handle.source_path).read_bytes())
    else:
        user_path.write_text(source_text)

    wrapper_filename = "tau_py_wrapper.py"
    wrapper_path = workload_dir / wrapper_filename
    wrapper_path.write_text(render_wrapper(user_filename))

    extra_specs = [f"{user_filename}:{user_filename}"]
    staged_roles: dict[str, tuple[str, Optional[str]]] = {
        user_filename: ("user-module", user_filename),
        wrapper_filename: ("entrypoint", "train.py"),
    }
    for source, destination in extra_scripts:
        destination = str(destination)
        staged_path = workload_dir / destination
        staged_path.write_bytes(pathlib.Path(source).read_bytes())
        extra_specs.append(f"{destination}:{destination}")
        staged_roles[destination] = ("extra-script", destination)

    params = handle._params
    if kind == "train":
        generated = _generated_run_config(
            manifest,
            wrapper_path=wrapper_filename,
            workload_kind=params.workload_kind or DEFAULT_WORKLOAD_KIND,
            extra_scripts=extra_specs,
            namespace=overrides.namespace or params.namespace or "ray",
            team=params.team,
            preset=params.preset,
            lane=params.lane,
            queue=overrides.queue or params.queue,
            gpu_class=overrides.gpu_class or params.gpu_class,
            gpu_resource_mode=params.gpu_resource_mode,
            node_selector=(
                overrides.node_selector
                if overrides.node_selector is not None
                else params.node_selector
            ),
            disable_default_priorities=overrides.disable_default_priorities,
            smoke_pairs=params.smoke_pairs or None,
            profiler=overrides.profiler,
            profile_rank=overrides.profile_rank,
            profile_warmup=overrides.profile_warmup,
            profile_duration=overrides.profile_duration,
        )
    else:
        generated = _generated_run_config(
            manifest,
            wrapper_path=wrapper_filename,
            workload_kind=EVAL_WORKLOAD_KIND,
            extra_scripts=extra_specs,
            namespace=overrides.namespace or params.namespace or "ray",
            team=params.team,
            preset=params.preset,
            gpu_class=overrides.gpu_class or params.gpu_class,
            gpu_resource_mode=params.gpu_resource_mode,
            node_selector=(
                overrides.node_selector
                if overrides.node_selector is not None
                else params.node_selector
            ),
            disable_default_priorities=overrides.disable_default_priorities,
            upstream_checkpoint=upstream_checkpoint,
        )

    config_filename = "tau.yaml"
    config_path = workload_dir / config_filename
    _write_canonical_yaml(config_path, generated)
    staged_roles[config_filename] = ("config", None)

    file_records = []
    for filename in sorted(staged_roles):
        role, destination = staged_roles[filename]
        path = workload_dir / filename
        raw = path.read_bytes()
        record: dict[str, Any] = {
            "path": (rel_dir / filename).as_posix(),
            "role": role,
            "sha256": hashlib.sha256(raw).hexdigest(),
            "size": len(raw),
        }
        if destination:
            record["staged_as"] = destination
        file_records.append(record)

    record = {
        "attribute": attribute,
        "config": (rel_dir / config_filename).as_posix(),
        "files": file_records,
        "kind": kind,
        "name": name,
        "namespace": overrides.namespace or params.namespace or "ray",
        "resource_name": _resource_name(handle),
    }
    if after:
        record["after"] = after
    if upstream_checkpoint:
        record["upstream_checkpoint"] = upstream_checkpoint
    if secret_sources:
        record["generated_secret"] = secret_name
        record["secret_sources"] = _secret_source_records(secret_sources)
    return record


def _resolve_build_upstream(
    train_handle: Any,
    eval_handle: Any,
    explicit: Optional[str],
) -> dict[str, str]:
    if eval_handle is None:
        return {}
    if train_handle is not None:
        train_name = str(train_handle._params.name)
        if eval_handle.after and eval_handle.after != train_name:
            raise BuildArtifactError(
                f"tau python build: @tau.eval after={eval_handle.after!r} does not "
                f"match @tau.train name={train_name!r}"
            )
        artifact = (train_handle.manifest().get("artifacts") or {}).get(
            "checkpoint",
            DEFAULT_CHECKPOINT_ARTIFACT,
        )
        artifact = validate_checkpoint_artifact(
            artifact,
            subject="tau python build: train manifest artifacts.checkpoint",
            error_type=BuildArtifactError,
        )
        checkpoint = (
            pathlib.PurePosixPath("/data/checkpoints")
            / "finetunes"
            / train_name
            / "artifacts"
            / artifact
        ).as_posix()
        return {"after": train_name, "checkpoint": checkpoint}
    if eval_handle.after:
        raise BuildArtifactError(
            f"tau python build: @tau.eval references after={eval_handle.after!r} "
            "but no @tau.train handle is defined"
        )
    if not explicit:
        raise BuildArtifactError(
            "tau python build: eval-only workflows require --upstream-checkpoint "
            "so the exported production intent is self-contained"
        )
    return {"checkpoint": explicit}


def _validate_handle_counts(
    train_handles: Sequence[tuple[str, Any]],
    eval_handles: Sequence[tuple[str, Any]],
) -> None:
    if len(train_handles) > 1:
        names = ", ".join(name for name, _ in train_handles)
        raise BuildArtifactError(
            f"tau python build: multiple @tau.train handles found ({names}); "
            "v1 supports at most one per file"
        )
    if len(eval_handles) > 1:
        names = ", ".join(name for name, _ in eval_handles)
        raise BuildArtifactError(
            f"tau python build: multiple @tau.eval handles found ({names}); "
            "v1 supports at most one per file"
        )


def _resource_name(handle: Any) -> str:
    extra_manifest = getattr(handle, "_extra_manifest", {}) or {}
    naming = extra_manifest.get("resource_naming")
    prefix = naming.get("prefix") if isinstance(naming, Mapping) else None
    return f"{prefix or 'tau'}-{handle._params.name}"


def _secret_source_records(sources: Mapping[str, SecretSource]) -> list[dict[str, Any]]:
    records = []
    for env_name in sorted(sources):
        source = sources[env_name]
        locator = (
            {"kind": "env", "name": source.env}
            if source.env
            else {"kind": "file", "path": source.path}
        )
        records.append(
            {
                "env_name": env_name,
                "key": source.key,
                "source": locator,
            }
        )
    return records


def _secret_sources_from_records(records: Sequence[Mapping[str, Any]]) -> dict[str, SecretSource]:
    sources: dict[str, SecretSource] = {}
    for record in records:
        env_name = str(record.get("env_name") or "")
        key = str(record.get("key") or "")
        locator = record.get("source")
        if not env_name or not key or not isinstance(locator, Mapping):
            raise BuildArtifactError("invalid source-backed secret metadata in Tau build")
        kind = locator.get("kind")
        if kind == "env":
            value = str(locator.get("name") or "")
            source = SecretSource(key=key, env=value)
        elif kind == "file":
            value = str(locator.get("path") or "")
            source = SecretSource(key=key, path=value)
        else:
            raise BuildArtifactError(
                f"invalid source-backed secret kind for {env_name!r}: {kind!r}"
            )
        if env_name in sources:
            raise BuildArtifactError(f"duplicate source-backed secret env name {env_name!r}")
        sources[env_name] = source
    return sources


def _write_canonical_yaml(path: pathlib.Path, value: Mapping[str, Any]) -> None:
    try:
        rendered = yaml.safe_dump(
            dict(value),
            allow_unicode=True,
            default_flow_style=False,
            line_break="\n",
            sort_keys=True,
        )
    except yaml.YAMLError as exc:
        raise BuildArtifactError(f"cannot serialize deterministic Tau build YAML: {exc}") from exc
    path.write_text(rendered, newline="\n")


def _check_output_destination(path: pathlib.Path, *, force: bool) -> None:
    if not path.exists() and not path.is_symlink():
        return
    if not force:
        raise BuildArtifactError(f"tau python build: output already exists: {path}; pass --force")
    if path.is_symlink() or not path.is_dir() or not (path / BUILD_MANIFEST).is_file():
        raise BuildArtifactError(
            f"tau python build: refusing to replace non-Tau-build output: {path}"
        )


def _artifact_file(root: pathlib.Path, value: Any, *, subject: str) -> pathlib.Path:
    raw = str(value or "")
    pure = pathlib.PurePosixPath(raw)
    if not raw or pure.is_absolute() or ".." in pure.parts or pure.as_posix() != raw:
        raise BuildArtifactError(f"invalid {subject} path in Tau build: {raw!r}")
    root_resolved = root.resolve()
    candidate = (root / pathlib.Path(*pure.parts)).resolve()
    try:
        candidate.relative_to(root_resolved)
    except ValueError as exc:
        raise BuildArtifactError(f"{subject} escapes Tau build root: {raw!r}") from exc
    if not candidate.is_file():
        raise BuildArtifactError(f"{subject} not found in Tau build: {raw!r}")
    return candidate


def _make_local_paths_absolute(config: dict[str, Any], base_dir: pathlib.Path) -> None:
    run = config.get("run")
    if isinstance(run, dict):
        for key in ("entrypoint", "script", "main_script"):
            if run.get(key):
                run[key] = _absolute_local_path(base_dir, str(run[key]))
    workflow = config.get("workflow")
    if not isinstance(workflow, dict):
        return
    for key in ("file", "script", "main_script"):
        if workflow.get(key):
            workflow[key] = _absolute_local_path(base_dir, str(workflow[key]))
    extra_scripts = []
    for spec in workflow.get("extra_scripts") or []:
        source, separator, destination = str(spec).partition(":")
        resolved = _absolute_local_path(base_dir, source)
        extra_scripts.append(resolved + (separator + destination if separator else ""))
    if extra_scripts:
        workflow["extra_scripts"] = extra_scripts


def _absolute_local_path(base_dir: pathlib.Path, value: str) -> str:
    path = pathlib.Path(value)
    if path.is_absolute():
        return str(path)
    return str((base_dir / path).resolve())
