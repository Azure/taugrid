# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Researcher-facing train/eval workload decorators for Tau."""

from __future__ import annotations

import functools
import inspect
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import warnings
from dataclasses import dataclass, field
from collections.abc import Mapping, Sequence
from typing import Any, Callable, Dict, List, Optional

import yaml

from tau._artifacts import DEFAULT_CHECKPOINT_ARTIFACT, validate_checkpoint_artifact
from tau._cluster import render_wrapper
from tau.config import (
    SecretSource,
    deep_merge,
    load_config,
    merge_runtime_env,
    normalize_mounts,
    runtime_env_entries,
)
from tau.entrypoints import call_staged_function

_RESERVED_MANIFEST_KEYS = frozenset({"schema_version", "name", "compute", "eval", "entrypoint"})

_USER_MODULE_FILENAME = "tau_user_module.py"
_REMOTE_ENTRYPOINT_PREFIX = "tau_entrypoint_"
_RESERVED_SCRIPT_DESTINATIONS = frozenset({"train.py", _USER_MODULE_FILENAME})

DEFAULT_WORKLOAD_KIND = "rayjob"


def _handle_call_signature(
    fn: Callable[..., Any],
    *,
    include_upstream_checkpoint: bool = False,
) -> inspect.Signature:
    signature = inspect.signature(fn)
    parameters = list(signature.parameters.values())
    if parameters:
        parameters = parameters[1:]

    if include_upstream_checkpoint:
        if any(parameter.name == "upstream_checkpoint" for parameter in parameters):
            raise ValueError(
                "tau-py: @tau.eval reserves upstream_checkpoint for local "
                "handle invocation; read it from ctx.upstream_checkpoint instead"
            )
        upstream_checkpoint = inspect.Parameter(
            "upstream_checkpoint",
            kind=inspect.Parameter.KEYWORD_ONLY,
            default=None,
        )
        insert_at = next(
            (
                index
                for index, parameter in enumerate(parameters)
                if parameter.kind == inspect.Parameter.VAR_KEYWORD
            ),
            len(parameters),
        )
        parameters.insert(insert_at, upstream_checkpoint)

    return signature.replace(parameters=parameters)


def _running_in_tau_job() -> bool:
    return bool(os.environ.get("TAU_DATA_DIR"))


def _refuse_cluster_submit(action: str = ".submit()") -> None:
    if _running_in_tau_job():
        raise RuntimeError(
            "tau-py: refusing to "
            + action
            + " from inside what looks like a cluster-submitted job "
            "(TAU_DATA_DIR is set). Submit from your local machine."
        )


def _check_dry_run(value: Optional[str]) -> None:
    if value is not None and value not in ("client", "server"):
        raise ValueError("dry_run must be 'client' or 'server'")


def _completed_process_kwargs(capture: bool) -> Dict[str, Any]:
    kwargs: Dict[str, Any] = {"check": not capture}
    if capture:
        kwargs["capture_output"] = True
        kwargs["text"] = True
    return kwargs


def _write_submission_files(
    tmp_path: pathlib.Path,
    *,
    name: str,
    manifest: Dict[str, Any],
    source_path: pathlib.Path,
    source_text: str | None = None,
    extra_scripts: Sequence[tuple[pathlib.Path, str]] = (),
) -> tuple[pathlib.Path, pathlib.Path, list[str]]:
    manifest_path = tmp_path / (name + ".yaml")
    with manifest_path.open("w") as f:
        yaml.safe_dump(manifest, f, sort_keys=False)

    user_filename = _USER_MODULE_FILENAME
    staged_user = tmp_path / user_filename
    if source_text is None:
        shutil.copyfile(source_path, staged_user)
    else:
        staged_user.write_text(source_text)

    wrapper_path = tmp_path / "tau_py_wrapper.py"
    wrapper_path.write_text(render_wrapper(user_filename))
    extra_args = [str(staged_user) + ":" + user_filename]
    for src, dest in extra_scripts:
        extra_args.append(str(src) + ":" + dest)
    return manifest_path, wrapper_path, extra_args


def _generated_run_config(
    manifest: Mapping[str, Any],
    *,
    wrapper_path: str | pathlib.Path,
    workload_kind: str,
    extra_scripts: Sequence[str],
    namespace: Optional[str],
    team: Optional[str],
    preset: Optional[str],
    lane: Optional[str] = None,
    queue: Optional[str] = None,
    gpu_class: Optional[str] = None,
    gpu_resource_mode: Optional[str] = None,
    node_selector: Any = None,
    disable_default_priorities: bool = False,
    smoke_pairs: Optional[int] = None,
    profiler: Optional[str] = None,
    profile_rank: Optional[str] = None,
    profile_warmup: Optional[str] = None,
    profile_duration: Optional[str] = None,
    secret_payload_path: Optional[str | pathlib.Path] = None,
    upstream_checkpoint: Optional[str] = None,
) -> Dict[str, Any]:
    generated = load_config(manifest)
    run_block = generated.setdefault("run", {})
    run_block["entrypoint"] = str(wrapper_path)
    run_block["workload_kind"] = workload_kind
    if smoke_pairs:
        run_block["smoke_pairs"] = int(smoke_pairs)
    policy = generated.setdefault("policy", {})
    for key, value in {
        "namespace": namespace,
        "team": team,
        "preset": preset,
        "lane": lane,
        "queue": queue,
        "gpu_class": gpu_class,
    }.items():
        if value:
            policy[key] = value
    selectors = _normalize_node_selectors(node_selector)
    if selectors:
        policy["node_selector"] = dict(item.split("=", 1) for item in selectors)
    if disable_default_priorities:
        policy["disable_default_priorities"] = True
    compute = generated.setdefault("compute", {})
    if gpu_resource_mode:
        compute["gpu_resource_mode"] = gpu_resource_mode
    workflow = generated.setdefault("workflow", {})
    if extra_scripts:
        workflow["extra_scripts"] = list(extra_scripts)
    if secret_payload_path is not None:
        workflow["secret_payload"] = str(secret_payload_path)
    if upstream_checkpoint:
        workflow["upstream_checkpoint"] = upstream_checkpoint
    profiler_block = generated.setdefault("profiler", {})
    for key, value in {
        "mode": profiler,
        "rank": profile_rank,
        "warmup": profile_warmup,
        "duration": profile_duration,
    }.items():
        if value:
            profiler_block[key] = value
    if not profiler_block:
        generated.pop("profiler", None)
    return generated


def _update_generated_run_config(
    manifest_path: pathlib.Path,
    *,
    wrapper_path: pathlib.Path,
    workload_kind: str,
    extra_scripts: Sequence[str],
    namespace: Optional[str],
    team: Optional[str],
    preset: Optional[str],
    lane: Optional[str] = None,
    queue: Optional[str] = None,
    gpu_class: Optional[str] = None,
    gpu_resource_mode: Optional[str] = None,
    node_selector: Any = None,
    disable_default_priorities: bool = False,
    smoke_pairs: Optional[int] = None,
    profiler: Optional[str] = None,
    profile_rank: Optional[str] = None,
    profile_warmup: Optional[str] = None,
    profile_duration: Optional[str] = None,
    secret_payload_path: Optional[pathlib.Path] = None,
    upstream_checkpoint: Optional[str] = None,
) -> None:
    with manifest_path.open() as f:
        manifest = yaml.safe_load(f) or {}
    generated = _generated_run_config(
        manifest,
        wrapper_path=wrapper_path,
        workload_kind=workload_kind,
        extra_scripts=extra_scripts,
        namespace=namespace,
        team=team,
        preset=preset,
        lane=lane,
        queue=queue,
        gpu_class=gpu_class,
        gpu_resource_mode=gpu_resource_mode,
        node_selector=node_selector,
        disable_default_priorities=disable_default_priorities,
        smoke_pairs=smoke_pairs,
        profiler=profiler,
        profile_rank=profile_rank,
        profile_warmup=profile_warmup,
        profile_duration=profile_duration,
        secret_payload_path=secret_payload_path,
        upstream_checkpoint=upstream_checkpoint,
    )
    with manifest_path.open("w") as f:
        yaml.safe_dump(generated, f, sort_keys=False)


def _job_secret_name(manifest: Dict[str, Any]) -> str:
    name = str(manifest.get("name") or "")
    prefix = "tau"
    raw_naming = manifest.get("resource_naming")
    if isinstance(raw_naming, Mapping) and raw_naming.get("prefix"):
        prefix = str(raw_naming["prefix"])
    return f"{prefix}-{name}-secrets"


def _collect_secret_sources(env: Mapping[str, Any] | Sequence[Any] | None) -> Dict[str, SecretSource]:
    if not isinstance(env, Mapping):
        return {}
    sources: Dict[str, SecretSource] = {}
    for env_name, value in env.items():
        if isinstance(value, SecretSource):
            sources[str(env_name)] = value
    return sources


def _collect_manifest_secret_sources(manifest: Mapping[str, Any]) -> Dict[str, SecretSource]:
    runtime = manifest.get("runtime")
    if not isinstance(runtime, Mapping):
        return {}
    sources: Dict[str, SecretSource] = {}
    for item in runtime.get("env") or []:
        if not isinstance(item, Mapping):
            continue
        value_from = item.get("valueFrom")
        if not isinstance(value_from, Mapping):
            continue
        source = value_from.get("tauSecretSource")
        if not isinstance(source, Mapping):
            continue
        env_name = str(item.get("name") or "")
        key = str(source.get("key") or env_name)
        env = source.get("env")
        path = source.get("path")
        sources[env_name] = SecretSource(
            key=key,
            env=str(env) if env else None,
            path=str(path) if path else None,
        )
    return sources


@dataclass(frozen=True)
class _ResolvedSecretValues:
    string_data: Dict[str, str]
    env_to_key: Dict[str, str]


def _resolve_secret_payload(
    manifest: Dict[str, Any],
    env: Mapping[str, Any] | Sequence[Any] | None,
) -> tuple[Dict[str, Any], Dict[str, Any]]:
    rewritten, sources, secret_name = _prepare_secret_references(manifest, env)
    if not sources or secret_name is None:
        return rewritten, {}
    values = _resolve_secret_values(sources)
    return rewritten, _build_secret_payload(secret_name, values.string_data)


def _prepare_secret_references(
    manifest: Dict[str, Any],
    env: Mapping[str, Any] | Sequence[Any] | None,
) -> tuple[Dict[str, Any], Dict[str, SecretSource], Optional[str]]:
    sources = _collect_secret_sources(env)
    sources.update(_collect_manifest_secret_sources(manifest))
    if not sources:
        return manifest, {}, None
    secret_name = _job_secret_name(manifest)
    env_to_key = {env_name: source.key for env_name, source in sources.items()}
    out = _rewrite_generated_secret_refs(manifest, secret_name, env_to_key)
    return out, sources, secret_name


def _resolve_secret_values(sources: Mapping[str, SecretSource]) -> _ResolvedSecretValues:
    string_data: Dict[str, str] = {}
    env_to_key: Dict[str, str] = {}
    for env_name, source in sources.items():
        string_data[source.key] = _read_secret_source_value(env_name, source)
        env_to_key[env_name] = source.key
    return _ResolvedSecretValues(string_data=string_data, env_to_key=env_to_key)


def _read_secret_source_value(env_name: str, source: SecretSource) -> str:
    if source.env:
        if source.env not in os.environ:
            raise ValueError(
                "tau-py: secret env source %r for %s is not set"
                % (source.env, env_name)
            )
        return os.environ[source.env]
    assert source.path is not None
    path = pathlib.Path(source.path).expanduser()
    if not path.exists():
        raise ValueError(
            "tau-py: secret file source %s for %s does not exist"
            % (path, env_name)
        )
    return path.read_text().rstrip("\n")


def _build_secret_payload(secret_name: str, string_data: Mapping[str, str]) -> Dict[str, Any]:
    return {"name": secret_name, "stringData": dict(string_data)}


def _rewrite_generated_secret_refs(
    manifest: Dict[str, Any],
    secret_name: str,
    env_to_key: Mapping[str, str],
) -> Dict[str, Any]:
    if not env_to_key:
        return manifest
    out = dict(manifest)
    runtime = _runtime_block(out)
    entries = []
    for item in runtime.get("env") or []:
        if not isinstance(item, Mapping):
            entries.append(item)
            continue
        entry = dict(item)
        env_name = str(entry.get("name") or "")
        if env_name in env_to_key:
            entry["valueFrom"] = {
                "secretKeyRef": {
                    "name": secret_name,
                    "key": env_to_key[env_name],
                }
            }
            entry.pop("value", None)
        entries.append(entry)
    runtime["env"] = entries
    out["runtime"] = runtime
    return out


def _write_job_secret_file(tmp_path: pathlib.Path, payload: Mapping[str, Any]) -> Optional[pathlib.Path]:
    if not payload:
        return None
    path = tmp_path / "tau-job-secrets.json"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    fd = os.open(path, flags, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(payload, f)
    return path


def _append_optional_flag(argv: List[str], flag: str, value: Any) -> None:
    if value:
        argv.extend([flag, str(value)])


def _normalize_node_selectors(selectors: Any) -> List[str]:
    if selectors is None:
        return []
    if isinstance(selectors, Mapping):
        raw = [str(k) + "=" + str(v) for k, v in selectors.items()]
    elif isinstance(selectors, str):
        raw = [selectors]
    else:
        raw = [str(v) for v in selectors]
    out: List[str] = []
    for item in raw:
        key, sep, value = item.partition("=")
        if sep != "=" or not key.strip() or not value.strip():
            raise ValueError(
                "tau-py: node_selector entries must be key=value with non-empty key and value"
            )
        out.append(key.strip() + "=" + value.strip())
    return out


def _require_runtime_pip(manifest: Dict[str, Any], submit_name: str) -> None:
    runtime_block = manifest.get("runtime") if isinstance(manifest, dict) else None
    if isinstance(runtime_block, dict) and runtime_block.get("pip"):
        return
    raise ValueError(
        "tau-py: %r has no runtime.pip — declare your trainer's pip "
        "deps via extra_manifest, e.g. "
        "@tau.train(..., extra_manifest={\"runtime\": {\"pip\": "
        "[\"torch==2.4.0\", \"transformers==4.45.0\"]}}). tau ships no "
        "default pip list." % submit_name
    )


def _validate_checkpoint_artifact(path: str) -> str:
    return validate_checkpoint_artifact(
        path,
        subject="tau-py: checkpoint_artifact",
        error_type=ValueError,
    )


def _with_checkpoint_artifact(manifest: Dict[str, Any], checkpoint_artifact: str) -> Dict[str, Any]:
    artifact_path = _validate_checkpoint_artifact(checkpoint_artifact)
    out = dict(manifest)
    raw_artifacts = out.get("artifacts") or {}
    if not isinstance(raw_artifacts, dict):
        raise ValueError("tau-py: extra_manifest['artifacts'] must be a dict")
    artifacts = dict(raw_artifacts)
    existing = artifacts.get("checkpoint")
    if existing is not None and existing != artifact_path:
        raise ValueError(
            "tau-py: checkpoint_artifact=%r conflicts with "
            "extra_manifest['artifacts']['checkpoint']=%r"
            % (artifact_path, existing)
        )
    artifacts["checkpoint"] = artifact_path
    out["artifacts"] = artifacts
    return out


def _with_runtime_pip(manifest: Dict[str, Any], runtime_pip: Optional[Sequence[str]]) -> Dict[str, Any]:
    if runtime_pip is None:
        return manifest
    out = dict(manifest)
    runtime = _runtime_block(out)
    pip = [str(pkg) for pkg in runtime_pip]
    if not pip or any(not pkg.strip() for pkg in pip):
        raise ValueError("tau-py: runtime_pip must contain at least one non-empty package")
    runtime["pip"] = pip
    out["runtime"] = runtime
    return out


def _with_runtime_env(
    manifest: Dict[str, Any],
    env: Mapping[str, Any] | Sequence[Any] | None,
) -> Dict[str, Any]:
    entries = runtime_env_entries(env)
    if not entries:
        return manifest
    out = dict(manifest)
    runtime = _runtime_block(out)
    runtime["env"] = merge_runtime_env(runtime.get("env"), entries)
    out["runtime"] = runtime
    return out


def _storage_block(manifest: Dict[str, Any]) -> Dict[str, Any]:
    raw_storage = manifest.get("storage") or {}
    if not isinstance(raw_storage, dict):
        raise ValueError("tau-py: config/extra_manifest['storage'] must be a dict")
    return dict(raw_storage)


def _with_storage_mounts(manifest: Dict[str, Any], mounts: Optional[Sequence[Any]]) -> Dict[str, Any]:
    normalized = normalize_mounts(mounts)
    if not normalized:
        return manifest
    out = dict(manifest)
    storage = _storage_block(out)
    existing = storage.get("mounts") or []
    if not isinstance(existing, list):
        raise ValueError("tau-py: storage.mounts must be a list")
    storage["mounts"] = [*existing, *normalized]
    out["storage"] = storage
    return out


def _with_data_pvc(
    manifest: Dict[str, Any],
    data_pvc: Optional[str],
    *,
    override: bool = False,
) -> Dict[str, Any]:
    if data_pvc is None:
        return manifest
    pvc = _nonblank_text(data_pvc, "data_pvc")
    out = dict(manifest)
    storage = _storage_block(out)
    existing = storage.get("data_pvc")
    if existing is not None and str(existing) != pvc and not override:
        raise ValueError(
            "tau-py: data_pvc=%r conflicts with existing storage.data_pvc=%r"
            % (pvc, existing)
        )
    storage["data_pvc"] = pvc
    out["storage"] = storage
    return out


def _positive_int(value: Optional[int], name: str) -> Optional[int]:
    if value is None:
        return None
    normalized = int(value)
    if normalized < 1:
        raise ValueError("tau-py: %s must be >= 1 (got %s)" % (name, normalized))
    return normalized


def _nonblank_text(value: Optional[str], name: str) -> Optional[str]:
    if value is None:
        return None
    normalized = str(value)
    if not normalized or normalized.strip() != normalized:
        raise ValueError("tau-py: %s must not be empty or have surrounding whitespace" % name)
    return normalized


def _compute_resource_overrides(
    *,
    cpu_request: Optional[int] = None,
    memory_request: Optional[str] = None,
    cpu_limit: Optional[int] = None,
    memory_limit: Optional[str] = None,
    worker_cpu_request: Optional[int] = None,
    worker_memory_request: Optional[str] = None,
    worker_cpu_limit: Optional[int] = None,
    worker_memory_limit: Optional[str] = None,
) -> Dict[str, Any]:
    raw = {
        "cpus": _positive_int(cpu_request, "cpu_request"),
        "memory": _nonblank_text(memory_request, "memory_request"),
        "cpu_limit": _positive_int(cpu_limit, "cpu_limit"),
        "memory_limit": _nonblank_text(memory_limit, "memory_limit"),
        "worker_cpus": _positive_int(worker_cpu_request, "worker_cpu_request"),
        "worker_memory": _nonblank_text(worker_memory_request, "worker_memory_request"),
        "worker_cpu_limit": _positive_int(worker_cpu_limit, "worker_cpu_limit"),
        "worker_memory_limit": _nonblank_text(worker_memory_limit, "worker_memory_limit"),
    }
    return {k: v for k, v in raw.items() if v is not None}


def _compute_block(manifest: Dict[str, Any]) -> Dict[str, Any]:
    raw_compute = manifest.get("compute") or {}
    if not isinstance(raw_compute, dict):
        raise ValueError("tau-py: config['compute'] must be a dict")
    return dict(raw_compute)


def _with_compute_resources(
    manifest: Dict[str, Any],
    resources: Mapping[str, Any] | None,
) -> Dict[str, Any]:
    if not resources:
        return manifest
    out = dict(manifest)
    compute = _compute_block(out)
    for key, value in resources.items():
        if value is not None:
            compute[str(key)] = value
    out["compute"] = compute
    return out


def _runtime_block(manifest: Dict[str, Any]) -> Dict[str, Any]:
    raw_runtime = manifest.get("runtime") or {}
    if not isinstance(raw_runtime, dict):
        raise ValueError("tau-py: config/extra_manifest['runtime'] must be a dict")
    return dict(raw_runtime)


def _validate_model_name(name: str) -> str:
    value = name.strip()
    if value != name or not value:
        raise ValueError("tau-py: model name must not be empty or have whitespace")
    if value[0] == "-" or value[-1] == "-":
        raise ValueError("tau-py: model name must use lowercase alphanumerics with internal hyphens")
    for ch in value:
        if not (ch.isdigit() or ("a" <= ch <= "z") or ch == "-"):
            raise ValueError("tau-py: model name must use lowercase alphanumerics with internal hyphens")
    return value


def _model_metadata(
    *,
    model: Optional[str],
    base_model: Optional[str],
    task: Optional[str],
    tags: Optional[Dict[str, str]],
    primary_metric: Optional[str],
    metric_direction: Optional[str],
) -> Dict[str, Any]:
    metadata: Dict[str, Any] = {}
    if model:
        metadata["name"] = _validate_model_name(model)
    if base_model:
        metadata["base"] = base_model
    if task:
        metadata["task"] = task
    if tags:
        normalized_tags: Dict[str, str] = {}
        for key, value in tags.items():
            if not isinstance(key, str) or not key.strip() or key.strip() != key:
                raise ValueError("tau-py: model tag keys must be non-empty strings without surrounding whitespace")
            if "/" in key or ".." in key:
                raise ValueError("tau-py: model tag keys must not contain '/' or '..'")
            if not isinstance(value, str) or value.strip() != value:
                raise ValueError("tau-py: model tag values must be strings without surrounding whitespace")
            normalized_tags[key] = value
        metadata["tags"] = normalized_tags
    if primary_metric:
        if primary_metric.strip() != primary_metric or ".." in primary_metric:
            raise ValueError("tau-py: primary_metric must not have whitespace or '..'")
        metadata["primary_metric"] = primary_metric
    if metric_direction:
        if metric_direction not in ("lower", "higher"):
            raise ValueError("tau-py: metric_direction must be 'lower' or 'higher'")
        metadata["metric_direction"] = metric_direction
    return metadata


def _with_model_metadata(manifest: Dict[str, Any], metadata: Dict[str, Any]) -> Dict[str, Any]:
    if not metadata:
        return manifest
    out = dict(manifest)
    raw_model = out.get("model") or {}
    if not isinstance(raw_model, dict):
        raise ValueError("tau-py: extra_manifest['model'] must be a dict")
    model_block = dict(raw_model)
    for key, value in metadata.items():
        if key == "tags":
            raw_tags = model_block.get("tags") or {}
            if not isinstance(raw_tags, dict):
                raise ValueError("tau-py: extra_manifest['model']['tags'] must be a dict")
            tags_block = dict(raw_tags)
            for tag_key, tag_value in value.items():
                existing = tags_block.get(tag_key)
                if existing is not None and existing != tag_value:
                    raise ValueError(
                        "tau-py: model tag %r conflicts with extra_manifest value %r"
                        % (tag_key, existing)
                    )
                tags_block[tag_key] = tag_value
            model_block["tags"] = tags_block
            continue
        existing = model_block.get(key)
        if existing is not None and existing != value:
            raise ValueError(
                "tau-py: model metadata %r=%r conflicts with extra_manifest value %r"
                % (key, value, existing)
            )
        model_block[key] = value
    out["model"] = model_block
    return out


@dataclass(frozen=True)
class _EntrypointSpec:
    local_script: pathlib.Path | None
    remote_script: str
    function: str
    args: tuple[Any, ...]
    kwargs: Dict[str, Any]
    pass_ctx: bool
    staged_scripts: tuple[tuple[pathlib.Path, str], ...]

    def manifest_block(self) -> Dict[str, Any]:
        return {
            "script": self.remote_script,
            "function": self.function,
            "args": list(self.args),
            "kwargs": dict(self.kwargs),
            "pass_ctx": bool(self.pass_ctx),
        }


def _parse_entrypoint(value: str | pathlib.Path) -> tuple[str, str]:
    raw = str(value).strip()
    if not raw:
        raise ValueError("tau-py: entrypoint must not be empty")
    script, sep, function = raw.rpartition(":")
    if not sep:
        return raw, "main"
    if not script:
        raise ValueError("tau-py: entrypoint must be PATH.py[:function]")
    if not function or not function.isidentifier():
        raise ValueError("tau-py: entrypoint function must be a Python identifier")
    return script, function


def _jsonable_entrypoint_payload(
    args: Sequence[Any] | None,
    kwargs: Mapping[str, Any] | None,
) -> tuple[tuple[Any, ...], Dict[str, Any]]:
    raw_args = list(args or [])
    raw_kwargs = dict(kwargs or {})
    try:
        encoded = json.dumps({"args": raw_args, "kwargs": raw_kwargs})
        decoded = json.loads(encoded)
    except (TypeError, ValueError) as exc:
        raise ValueError("tau-py: entrypoint_args and entrypoint_kwargs must be JSON-serializable") from exc
    return tuple(decoded["args"]), dict(decoded["kwargs"])


def _script_dest(path: pathlib.Path, *, prefix: str = "") -> str:
    name = path.name
    if not name or name in {".", ".."}:
        raise ValueError(f"tau-py: script path must have a filename: {path}")
    dest = prefix + name
    if "/" in dest or "\\" in dest or dest in _RESERVED_SCRIPT_DESTINATIONS:
        raise ValueError(f"tau-py: script destination {dest!r} is reserved or invalid")
    return dest


def _normalize_extra_scripts(extra_scripts: Sequence[str | pathlib.Path] | None) -> list[tuple[pathlib.Path, str]]:
    staged: list[tuple[pathlib.Path, str]] = []
    seen = set(_RESERVED_SCRIPT_DESTINATIONS)
    for item in extra_scripts or ():
        path = pathlib.Path(item).expanduser().resolve(strict=False)
        if not path.is_file():
            raise FileNotFoundError(f"tau-py: extra script not found: {path}")
        dest = _script_dest(path)
        if dest in seen:
            raise ValueError(f"tau-py: duplicate staged script destination {dest!r}")
        seen.add(dest)
        staged.append((path, dest))
    return staged


def _normalize_entrypoint_spec(
    entrypoint: str | pathlib.Path,
    *,
    args: Sequence[Any] | None,
    kwargs: Mapping[str, Any] | None,
    pass_ctx: bool,
    extra_scripts: Sequence[str | pathlib.Path] | None,
) -> _EntrypointSpec:
    script_text, function = _parse_entrypoint(entrypoint)
    script = pathlib.Path(script_text).expanduser()
    payload_args, payload_kwargs = _jsonable_entrypoint_payload(args, kwargs)
    staged = _normalize_extra_scripts(extra_scripts)
    staged_by_dest = {dest for _, dest in staged}

    if script.is_file():
        local_script = script.resolve(strict=False)
        dest = _script_dest(local_script, prefix=_REMOTE_ENTRYPOINT_PREFIX)
        while dest in staged_by_dest or dest in _RESERVED_SCRIPT_DESTINATIONS:
            dest = _REMOTE_ENTRYPOINT_PREFIX + dest
        remote_script = "/script/" + dest
        staged = [(local_script, dest)] + staged
    elif script.is_absolute():
        local_script = None
        remote_script = str(script)
    else:
        raise FileNotFoundError(
            f"tau-py: relative entrypoint {script} was not found from {pathlib.Path.cwd()}; "
            "pass an existing local file to stage it or an absolute PVC path available in the pod"
        )

    return _EntrypointSpec(
        local_script=local_script,
        remote_script=remote_script,
        function=function,
        args=payload_args,
        kwargs=payload_kwargs,
        pass_ctx=bool(pass_ctx),
        staged_scripts=tuple(staged),
    )


def _with_entrypoint_metadata(manifest: Dict[str, Any], spec: _EntrypointSpec | None) -> Dict[str, Any]:
    if spec is None:
        return manifest
    if "entrypoint" in manifest:
        raise ValueError("tau-py: manifest entrypoint is owned by tau.train(entrypoint=...)")
    out = dict(manifest)
    out["entrypoint"] = spec.manifest_block()
    return out


def _entrypoint_local_fn(spec: _EntrypointSpec) -> Callable[["Ctx"], Any]:
    def _tau_entrypoint_train(ctx: "Ctx") -> Any:
        script = spec.local_script or pathlib.Path(spec.remote_script)
        args = (ctx, *spec.args) if spec.pass_ctx else spec.args
        return call_staged_function(script, spec.function, *args, **spec.kwargs)

    _tau_entrypoint_train.__name__ = "_tau_entrypoint_train"
    return _tau_entrypoint_train


def _render_entrypoint_user_module() -> str:
    return '''import tau


@tau.train(name="tau-entrypoint")
def _tau_entrypoint_train(ctx):
    spec = ctx.manifest.get("entrypoint") or {}
    script = spec.get("script")
    function = spec.get("function") or "main"
    args = list(spec.get("args") or [])
    kwargs = dict(spec.get("kwargs") or {})
    if not script:
        raise RuntimeError("tau-py entrypoint mode requires manifest.entrypoint.script")
    if spec.get("pass_ctx"):
        return tau.call_staged_function(script, function, ctx, *args, **kwargs)
    return tau.call_staged_function(script, function, *args, **kwargs)
'''


@dataclass
class Ctx:
    """Runtime context passed to train/eval functions."""

    name: str
    gpus: int
    workers: int = 1
    smoke_pairs: int = 0
    manifest: Dict[str, Any] = field(default_factory=dict)
    manifest_path: Optional[pathlib.Path] = None
    data_dir: pathlib.Path = field(default_factory=lambda: pathlib.Path.cwd())
    hot_dir: pathlib.Path = field(default_factory=lambda: pathlib.Path.cwd())
    datasets_dir: pathlib.Path = field(default_factory=lambda: pathlib.Path.cwd() / "datasets")
    checkpoints_dir: pathlib.Path = field(default_factory=lambda: pathlib.Path.cwd() / "checkpoints")
    durable_datasets_dir: pathlib.Path = field(default_factory=lambda: pathlib.Path.cwd() / "datasets")
    durable_checkpoints_dir: pathlib.Path = field(default_factory=lambda: pathlib.Path.cwd() / "checkpoints")
    upstream_checkpoint: Optional[pathlib.Path] = None
    resume_from: Optional[pathlib.Path] = None
    storage_hot_status: str = "local"
    storage_hot_reason: str = ""
    storage_hot_write_mbps: Optional[float] = None
    is_remote: bool = False


@dataclass(frozen=True)
class _TrainParams:
    name: str
    gpus: int
    workers: int
    smoke_pairs: int
    workload_kind: str = DEFAULT_WORKLOAD_KIND
    checkpoint_artifact: str = DEFAULT_CHECKPOINT_ARTIFACT
    model_metadata: Dict[str, Any] = field(default_factory=dict)
    namespace: Optional[str] = None
    team: Optional[str] = None
    preset: Optional[str] = None
    lane: Optional[str] = None
    queue: Optional[str] = None
    gpu_class: Optional[str] = None
    gpu_resource_mode: Optional[str] = None
    node_selector: Any = None
    data_pvc: Optional[str] = None
    compute_resources: Dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class _EvalParams:
    name: str
    gpus: int
    cpu_workers: int
    after: Optional[str] = None
    gpu_class: Optional[str] = None
    gpu_resource_mode: Optional[str] = None
    node_selector: Any = None
    data_pvc: Optional[str] = None
    compute_resources: Dict[str, Any] = field(default_factory=dict)
    namespace: Optional[str] = None
    team: Optional[str] = None
    preset: Optional[str] = None


class _TrainHandle:
    """Decorator handle with local call and remote submit paths."""

    _tau_train_handle = True

    def __init__(
        self,
        fn: Callable[[Ctx], Any],
        *,
        name: Optional[str] = None,
        gpus: Optional[int] = None,
        workers: Optional[int] = None,
        smoke_pairs: int = 0,
        namespace: Optional[str] = None,
        workload_kind: str = DEFAULT_WORKLOAD_KIND,
        team: Optional[str] = None,
        preset: Optional[str] = None,
        lane: Optional[str] = None,
        queue: Optional[str] = None,
        gpu_class: Optional[str] = None,
        gpu_resource_mode: Optional[str] = None,
        node_selector: Any = None,
        data_pvc: Optional[str] = None,
        cpu_request: Optional[int] = None,
        memory_request: Optional[str] = None,
        cpu_limit: Optional[int] = None,
        memory_limit: Optional[str] = None,
        worker_cpu_request: Optional[int] = None,
        worker_memory_request: Optional[str] = None,
        worker_cpu_limit: Optional[int] = None,
        worker_memory_limit: Optional[str] = None,
        checkpoint_artifact: str = DEFAULT_CHECKPOINT_ARTIFACT,
        model: Optional[str] = None,
        base_model: Optional[str] = None,
        task: Optional[str] = None,
        tags: Optional[Dict[str, str]] = None,
        primary_metric: Optional[str] = None,
        metric_direction: Optional[str] = None,
        config: Optional[Any] = None,
        runtime_pip: Optional[Sequence[str]] = None,
        env: Mapping[str, Any] | Sequence[Any] | None = None,
        mounts: Optional[Sequence[Any]] = None,
        extra_manifest: Optional[Dict[str, Any]] = None,
        entrypoint_spec: _EntrypointSpec | None = None,
        source_text: str | None = None,
        source_path: pathlib.Path | None = None,
    ):
        config_manifest = load_config(config)
        config_compute = config_manifest.get("compute") if isinstance(config_manifest.get("compute"), dict) else {}
        resolved_name = name or str(config_manifest.get("name") or "")
        if not resolved_name:
            raise ValueError("tau-py: @tau.train requires name=... or config['name']")
        resolved_gpus = int(gpus if gpus is not None else config_compute.get("gpus", 1))
        resolved_workers = int(workers if workers is not None else config_compute.get("workers", 1))
        if resolved_workers < 1:
            raise ValueError(
                "tau-py: workers must be >= 1 (got " + str(resolved_workers) + ")"
            )
        if lane == "eval":
            # @tau.train is for training shapes; @tau.eval is its own
            # decorator with the eval Kueue queue + GPU-actor + CPU-workers
            # topology. Allowing lane="eval" here would silently land a
            # multi-GPU train on the eval queue (wrong priority class,
            # wrong shape). The Go validator would catch it on submit, but
            # surfacing it at decoration time saves a round-trip.
            raise ValueError(
                "tau-py: @tau.train cannot use lane=\"eval\"; "
                "use @tau.eval for eval workloads."
            )
        self._fn = fn
        self._params = _TrainParams(
            name=resolved_name,
            gpus=resolved_gpus,
            workers=resolved_workers,
            smoke_pairs=smoke_pairs,
            namespace=namespace,
            workload_kind=workload_kind,
            team=team,
            preset=preset,
            lane=lane,
            queue=queue,
            gpu_class=gpu_class,
            gpu_resource_mode=gpu_resource_mode,
            node_selector=node_selector,
            data_pvc=_nonblank_text(data_pvc, "data_pvc"),
            compute_resources=_compute_resource_overrides(
                cpu_request=cpu_request,
                memory_request=memory_request,
                cpu_limit=cpu_limit,
                memory_limit=memory_limit,
                worker_cpu_request=worker_cpu_request,
                worker_memory_request=worker_memory_request,
                worker_cpu_limit=worker_cpu_limit,
                worker_memory_limit=worker_memory_limit,
            ),
            checkpoint_artifact=_validate_checkpoint_artifact(checkpoint_artifact),
            model_metadata=_model_metadata(
                model=model,
                base_model=base_model,
                task=task,
                tags=tags,
                primary_metric=primary_metric,
                metric_direction=metric_direction,
            ),
        )
        self._config_manifest = config_manifest
        self._runtime_pip = runtime_pip
        self._env = env
        self._mounts = mounts
        self._extra_manifest = extra_manifest or {}
        self._entrypoint_spec = entrypoint_spec
        self._source_text = source_text
        self._extra_scripts = tuple(entrypoint_spec.staged_scripts if entrypoint_spec else ())
        self._source_path = source_path or self._resolve_source_path(fn)
        functools.update_wrapper(self, fn, updated=())
        self.__signature__ = _handle_call_signature(fn)

    # ----- introspection ---------------------------------------------------

    @staticmethod
    def _resolve_source_path(fn: Callable) -> pathlib.Path:
        try:
            src = inspect.getfile(fn)
        except TypeError as e:  # builtins, etc.
            raise RuntimeError(
                "tau-py: cannot determine source file of training "
                "function; @train must decorate a function defined in a "
                ".py file (got " + repr(fn) + ")"
            ) from e
        return pathlib.Path(src).resolve()

    @property
    def source_path(self) -> pathlib.Path:
        return self._source_path

    def manifest(self) -> Dict[str, Any]:
        """The YAML-shaped dict tau's Go side will parse."""
        m: Dict[str, Any] = load_config(self._config_manifest)
        m.setdefault("schema_version", 1)
        compute = _compute_block(m)
        params = self._params
        compute["gpus"] = int(params.gpus)
        workers = int(params.workers)
        if workers > 1 or "workers" in compute:
            compute["workers"] = workers
        for key, value in params.compute_resources.items():
            compute[key] = value
        m["name"] = params.name
        m["compute"] = compute
        m.setdefault("eval", {})
        if self._extra_manifest:
            collisions = sorted(_RESERVED_MANIFEST_KEYS & self._extra_manifest.keys())
            if collisions:
                raise ValueError(
                    "tau-py: extra_manifest cannot override reserved keys "
                    + ", ".join(collisions)
                    + " (these are owned by the @train decorator kwargs to "
                    "keep the local Ctx and the submitted manifest in sync)"
                )
            m = deep_merge(m, self._extra_manifest)
        m = _with_runtime_pip(m, self._runtime_pip)
        m = _with_runtime_env(m, self._env)
        m = _with_data_pvc(m, params.data_pvc)
        m = _with_storage_mounts(m, self._mounts)
        m = _with_model_metadata(m, params.model_metadata)
        m = _with_entrypoint_metadata(m, self._entrypoint_spec)
        return _with_checkpoint_artifact(m, params.checkpoint_artifact)

    # ----- local execution -------------------------------------------------

    def __call__(self, *args, **kwargs):
        """Run the training function locally with a cwd-rooted Ctx.

        Useful for fast iteration / debugging on a workstation. No GPU
        scheduling, no manifest generation, no tau CLI involvement.
        """
        if self._entrypoint_spec is not None and len(args) == 1 and callable(args[0]) and not kwargs:
            raise TypeError(
                "tau.train(entrypoint=...) returns a train handle directly; "
                "do not use it as a decorator"
            )
        params = self._params
        ctx = Ctx(
            name=params.name,
            gpus=int(params.gpus),
            workers=int(params.workers),
            smoke_pairs=int(params.smoke_pairs),
            manifest=self.manifest(),
            is_remote=False,
        )
        return self._fn(ctx, *args, **kwargs)

    # ----- remote submission ----------------------------------------------

    def _submission_manifest(
        self,
        *,
        data_pvc: Optional[str],
        cpu_request: Optional[int],
        memory_request: Optional[str],
        cpu_limit: Optional[int],
        memory_limit: Optional[str],
        worker_cpu_request: Optional[int],
        worker_memory_request: Optional[str],
        worker_cpu_limit: Optional[int],
        worker_memory_limit: Optional[str],
    ) -> Dict[str, Any]:
        manifest = self.manifest()
        manifest = _with_data_pvc(manifest, data_pvc, override=True)
        manifest = _with_compute_resources(
            manifest,
            _compute_resource_overrides(
                cpu_request=cpu_request,
                memory_request=memory_request,
                cpu_limit=cpu_limit,
                memory_limit=memory_limit,
                worker_cpu_request=worker_cpu_request,
                worker_memory_request=worker_memory_request,
                worker_cpu_limit=worker_cpu_limit,
                worker_memory_limit=worker_memory_limit,
            ),
        )
        _require_runtime_pip(manifest, self._params.name)
        return manifest

    def _submit_manifest(
        self,
        *,
        data_pvc: Optional[str],
        cpu_request: Optional[int],
        memory_request: Optional[str],
        cpu_limit: Optional[int],
        memory_limit: Optional[str],
        worker_cpu_request: Optional[int],
        worker_memory_request: Optional[str],
        worker_cpu_limit: Optional[int],
        worker_memory_limit: Optional[str],
    ) -> tuple[Dict[str, Any], Dict[str, Any]]:
        manifest = self._submission_manifest(
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
        )
        manifest, secret_payload = _resolve_secret_payload(manifest, self._env)
        return manifest, secret_payload

    def _submit_argv(
        self,
        *,
        binary: str,
        manifest_path: pathlib.Path,
        wrapper_path: pathlib.Path,
        extra_scripts: Sequence[str],
        secret_payload_path: Optional[pathlib.Path],
        dry_run: Optional[str],
        kube_context: Optional[str],
        namespace: Optional[str],
        smoke_pairs: Optional[int],
        team: Optional[str],
        preset: Optional[str],
        lane: Optional[str],
        queue: Optional[str],
        gpu_class: Optional[str],
        gpu_resource_mode: Optional[str],
        node_selector: Any,
        profiler: Optional[str],
        profile_rank: Optional[str],
        profile_warmup: Optional[str],
        profile_duration: Optional[str],
        disable_default_priorities: bool = False,
    ) -> List[str]:
        params = self._params
        sp = smoke_pairs if smoke_pairs is not None else params.smoke_pairs
        _update_generated_run_config(
            manifest_path,
            wrapper_path=wrapper_path,
            workload_kind=params.workload_kind or DEFAULT_WORKLOAD_KIND,
            extra_scripts=extra_scripts,
            namespace=namespace or params.namespace,
            team=team or params.team,
            preset=preset or params.preset,
            lane=lane or params.lane,
            queue=queue or params.queue,
            gpu_class=gpu_class or params.gpu_class,
            gpu_resource_mode=gpu_resource_mode or params.gpu_resource_mode,
            node_selector=node_selector if node_selector is not None else params.node_selector,
            disable_default_priorities=disable_default_priorities,
            smoke_pairs=int(sp) if sp else None,
            profiler=profiler,
            profile_rank=profile_rank,
            profile_warmup=profile_warmup,
            profile_duration=profile_duration,
            secret_payload_path=secret_payload_path,
        )
        argv: List[str] = [binary, "run", "--config", str(manifest_path)]
        _append_optional_flag(argv, "--dry-run", dry_run)
        _append_optional_flag(argv, "--context", kube_context)
        return argv

    def submit(
        self,
        *,
        tau_binary: Optional[str] = None,
        dry_run: Optional[str] = None,
        kube_context: Optional[str] = None,
        namespace: Optional[str] = None,
        smoke_pairs: Optional[int] = None,
        team: Optional[str] = None,
        preset: Optional[str] = None,
        lane: Optional[str] = None,
        queue: Optional[str] = None,
        gpu_class: Optional[str] = None,
        gpu_resource_mode: Optional[str] = None,
        node_selector: Any = None,
        profiler: Optional[str] = None,
        profile_rank: Optional[str] = None,
        profile_warmup: Optional[str] = None,
        profile_duration: Optional[str] = None,
        data_pvc: Optional[str] = None,
        cpu_request: Optional[int] = None,
        memory_request: Optional[str] = None,
        cpu_limit: Optional[int] = None,
        memory_limit: Optional[str] = None,
        worker_cpu_request: Optional[int] = None,
        worker_memory_request: Optional[str] = None,
        worker_cpu_limit: Optional[int] = None,
        worker_memory_limit: Optional[str] = None,
        disable_default_priorities: bool = False,
        capture: bool = False,
    ) -> subprocess.CompletedProcess:
        """Synthesize a manifest + cluster wrapper, shell to tau CLI.

        Returns the CompletedProcess from the tau subcommand. Non-zero exit
        is surfaced via ``check=True`` (raises CalledProcessError) when
        ``capture=False`` (the default — output streams to stdout/stderr).
        With ``capture=True``, returns the completed process and the caller
        decides what to do with it.
        """
        _refuse_cluster_submit()
        _check_dry_run(dry_run)

        binary = tau_binary or _find_tau_binary()
        manifest, secret_payload = self._submit_manifest(
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
        )
        with tempfile.TemporaryDirectory(prefix="tau-py-") as tmp:
            tmp_path = pathlib.Path(tmp)
            secret_payload_path = _write_job_secret_file(tmp_path, secret_payload)
            manifest_path, wrapper_path, extra_scripts = _write_submission_files(
                tmp_path,
                name=self._params.name,
                manifest=manifest,
                source_path=self._source_path,
                source_text=self._source_text,
                extra_scripts=self._extra_scripts,
            )
            argv = self._submit_argv(
                binary=binary,
                manifest_path=manifest_path,
                wrapper_path=wrapper_path,
                extra_scripts=extra_scripts,
                secret_payload_path=secret_payload_path,
                dry_run=dry_run,
                kube_context=kube_context,
                namespace=namespace,
                smoke_pairs=smoke_pairs,
                team=team,
                preset=preset,
                lane=lane,
                queue=queue,
                gpu_class=gpu_class,
                gpu_resource_mode=gpu_resource_mode,
                node_selector=node_selector,
                profiler=profiler,
                profile_rank=profile_rank,
                profile_warmup=profile_warmup,
                profile_duration=profile_duration,
                disable_default_priorities=disable_default_priorities,
            )

            return subprocess.run(argv, **_completed_process_kwargs(capture))


def train(
    *,
    name: Optional[str] = None,
    gpus: Optional[int] = None,
    workers: Optional[int] = None,
    smoke_pairs: int = 0,
    namespace: Optional[str] = None,
    workload_kind: str = DEFAULT_WORKLOAD_KIND,
    team: Optional[str] = None,
    preset: Optional[str] = None,
    lane: Optional[str] = None,
    queue: Optional[str] = None,
    gpu_class: Optional[str] = None,
    gpu_resource_mode: Optional[str] = None,
    node_selector: Any = None,
    data_pvc: Optional[str] = None,
    cpu_request: Optional[int] = None,
    memory_request: Optional[str] = None,
    cpu_limit: Optional[int] = None,
    memory_limit: Optional[str] = None,
    worker_cpu_request: Optional[int] = None,
    worker_memory_request: Optional[str] = None,
    worker_cpu_limit: Optional[int] = None,
    worker_memory_limit: Optional[str] = None,
    checkpoint_artifact: str = DEFAULT_CHECKPOINT_ARTIFACT,
    model: Optional[str] = None,
    base_model: Optional[str] = None,
    task: Optional[str] = None,
    tags: Optional[Dict[str, str]] = None,
    primary_metric: Optional[str] = None,
    metric_direction: Optional[str] = None,
    config: Optional[Any] = None,
    runtime_pip: Optional[Sequence[str]] = None,
    env: Mapping[str, Any] | Sequence[Any] | None = None,
    mounts: Optional[Sequence[Any]] = None,
    extra_manifest: Optional[Dict[str, Any]] = None,
    entrypoint: str | pathlib.Path | None = None,
    entrypoint_args: Sequence[Any] | None = None,
    entrypoint_kwargs: Mapping[str, Any] | None = None,
    entrypoint_pass_ctx: bool = False,
    extra_scripts: Sequence[str | pathlib.Path] | None = None,
) -> Callable[[Callable[[Ctx], Any]], _TrainHandle] | _TrainHandle:
    """Register a Tau train job.

    Without ``entrypoint`` this is the decorator form:
    ``@tau.train(...); def train(ctx): ...``. With ``entrypoint`` it returns a
    handle directly and Tau adapts ``path.py:function`` into the same cluster
    wrapper/Ray Train contract.
    """

    if entrypoint is not None:
        spec = _normalize_entrypoint_spec(
            entrypoint,
            args=entrypoint_args,
            kwargs=entrypoint_kwargs,
            pass_ctx=entrypoint_pass_ctx,
            extra_scripts=extra_scripts,
        )
        source_path = spec.local_script or pathlib.Path(spec.remote_script)
        return _TrainHandle(
            _entrypoint_local_fn(spec),
            name=name,
            gpus=gpus,
            workers=workers,
            smoke_pairs=smoke_pairs,
            namespace=namespace,
            workload_kind=workload_kind,
            team=team,
            preset=preset,
            lane=lane,
            queue=queue,
            gpu_class=gpu_class,
            gpu_resource_mode=gpu_resource_mode,
            node_selector=node_selector,
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
            checkpoint_artifact=checkpoint_artifact,
            model=model,
            base_model=base_model,
            task=task,
            tags=tags,
            primary_metric=primary_metric,
            metric_direction=metric_direction,
            config=config,
            runtime_pip=runtime_pip,
            env=env,
            mounts=mounts,
            extra_manifest=extra_manifest,
            entrypoint_spec=spec,
            source_text=_render_entrypoint_user_module(),
            source_path=source_path,
        )

    if entrypoint_args or entrypoint_kwargs or entrypoint_pass_ctx or extra_scripts:
        raise ValueError("tau-py: entrypoint_* and extra_scripts require entrypoint=...")

    def decorator(fn: Callable[[Ctx], Any]) -> _TrainHandle:
        return _TrainHandle(
            fn,
            name=name,
            gpus=gpus,
            workers=workers,
            smoke_pairs=smoke_pairs,
            namespace=namespace,
            workload_kind=workload_kind,
            team=team,
            preset=preset,
            lane=lane,
            queue=queue,
            gpu_class=gpu_class,
            gpu_resource_mode=gpu_resource_mode,
            node_selector=node_selector,
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
            checkpoint_artifact=checkpoint_artifact,
            model=model,
            base_model=base_model,
            task=task,
            tags=tags,
            primary_metric=primary_metric,
            metric_direction=metric_direction,
            config=config,
            runtime_pip=runtime_pip,
            env=env,
            mounts=mounts,
            extra_manifest=extra_manifest,
        )

    return decorator


EVAL_WORKLOAD_KIND = "rayjob-eval"


class _EvalHandle:
    """Decorator handle for the eval RayJob shape."""

    _tau_eval_handle = True

    def __init__(
        self,
        fn: Callable[[Ctx], Any],
        *,
        name: str,
        after: Optional[str] = None,
        gpus: int = 1,
        cpu_workers: int = 1,
        gpu_class: Optional[str] = None,
        gpu_resource_mode: Optional[str] = None,
        node_selector: Any = None,
        data_pvc: Optional[str] = None,
        cpu_request: Optional[int] = None,
        memory_request: Optional[str] = None,
        cpu_limit: Optional[int] = None,
        memory_limit: Optional[str] = None,
        worker_cpu_request: Optional[int] = None,
        worker_memory_request: Optional[str] = None,
        worker_cpu_limit: Optional[int] = None,
        worker_memory_limit: Optional[str] = None,
        namespace: Optional[str] = None,
        team: Optional[str] = None,
        preset: Optional[str] = None,
        extra_manifest: Optional[Dict[str, Any]] = None,
    ):
        if cpu_workers < 1:
            raise ValueError(
                "tau-py: @tau.eval cpu_workers must be >= 1 (got "
                + str(cpu_workers)
                + "); the eval shape is a system head + 1 GPU actor worker + N CPU workers, "
                "and N=0 makes no sense (use @tau.train instead)"
            )
        if gpus < 1:
            raise ValueError(
                "tau-py: @tau.eval gpus must be >= 1 (got " + str(gpus) + ")"
            )
        self._fn = fn
        self._params = _EvalParams(
            name=name,
            after=after,
            gpus=gpus,
            cpu_workers=cpu_workers,
            gpu_class=gpu_class,
            gpu_resource_mode=gpu_resource_mode,
            node_selector=node_selector,
            data_pvc=_nonblank_text(data_pvc, "data_pvc"),
            compute_resources=_compute_resource_overrides(
                cpu_request=cpu_request,
                memory_request=memory_request,
                cpu_limit=cpu_limit,
                memory_limit=memory_limit,
                worker_cpu_request=worker_cpu_request,
                worker_memory_request=worker_memory_request,
                worker_cpu_limit=worker_cpu_limit,
                worker_memory_limit=worker_memory_limit,
            ),
            namespace=namespace,
            team=team,
            preset=preset,
        )
        self._extra_manifest = extra_manifest or {}
        self._source_path = _TrainHandle._resolve_source_path(fn)
        functools.update_wrapper(self, fn, updated=())
        self.__signature__ = _handle_call_signature(
            fn,
            include_upstream_checkpoint=True,
        )

    @property
    def source_path(self) -> pathlib.Path:
        return self._source_path

    @property
    def after(self) -> Optional[str]:
        """Name of the upstream @tau.train handle this eval depends on."""
        return self._params.after

    def manifest(self) -> Dict[str, Any]:
        """The YAML-shaped dict tau's Go side will parse for this eval.

        Eval manifests carry ``eval.cpu_workers`` (and optionally
        ``eval.upstream``) which the Go CLI reads via ``IsEval()`` to
        dispatch to the rayjob-eval template.
        """
        params = self._params
        compute: Dict[str, Any] = {"gpus": int(params.gpus)}
        for key, value in params.compute_resources.items():
            compute[key] = value
        eval_block: Dict[str, Any] = {
            "cpu_workers": int(params.cpu_workers),
        }
        if params.after:
            eval_block["upstream"] = params.after
        m: Dict[str, Any] = {
            "schema_version": 1,
            "name": params.name,
            "compute": compute,
            "eval": eval_block,
        }
        if self._extra_manifest:
            collisions = sorted(_RESERVED_MANIFEST_KEYS & self._extra_manifest.keys())
            if collisions:
                raise ValueError(
                    "tau-py: extra_manifest cannot override reserved keys "
                    + ", ".join(collisions)
                    + " (these are owned by the @eval decorator kwargs to "
                    "keep the local Ctx and the submitted manifest in sync)"
                )
            m.update(self._extra_manifest)
        m = _with_data_pvc(m, params.data_pvc)
        return m

    def __call__(self, *args, **kwargs):
        """Run the eval function locally with a cwd-rooted Ctx.

        Useful for fast iteration on a workstation with a checkpoint on
        disk. Pass ``upstream_checkpoint=Path(...)`` as a kwarg to point
        the local Ctx at a specific checkpoint file.
        """
        upstream = kwargs.pop("upstream_checkpoint", None)
        params = self._params
        ctx = Ctx(
            name=params.name,
            gpus=int(params.gpus),
            workers=1,
            manifest=self.manifest(),
            upstream_checkpoint=pathlib.Path(upstream) if upstream else None,
            is_remote=False,
        )
        return self._fn(ctx, *args, **kwargs)

    def _submission_manifest(
        self,
        *,
        data_pvc: Optional[str],
        cpu_request: Optional[int],
        memory_request: Optional[str],
        cpu_limit: Optional[int],
        memory_limit: Optional[str],
        worker_cpu_request: Optional[int],
        worker_memory_request: Optional[str],
        worker_cpu_limit: Optional[int],
        worker_memory_limit: Optional[str],
    ) -> Dict[str, Any]:
        manifest = self.manifest()
        manifest = _with_data_pvc(manifest, data_pvc, override=True)
        manifest = _with_compute_resources(
            manifest,
            _compute_resource_overrides(
                cpu_request=cpu_request,
                memory_request=memory_request,
                cpu_limit=cpu_limit,
                memory_limit=memory_limit,
                worker_cpu_request=worker_cpu_request,
                worker_memory_request=worker_memory_request,
                worker_cpu_limit=worker_cpu_limit,
                worker_memory_limit=worker_memory_limit,
            ),
        )
        _require_runtime_pip(manifest, self._params.name)
        return manifest

    def _submit_manifest(
        self,
        *,
        data_pvc: Optional[str],
        cpu_request: Optional[int],
        memory_request: Optional[str],
        cpu_limit: Optional[int],
        memory_limit: Optional[str],
        worker_cpu_request: Optional[int],
        worker_memory_request: Optional[str],
        worker_cpu_limit: Optional[int],
        worker_memory_limit: Optional[str],
    ) -> tuple[Dict[str, Any], Dict[str, Any]]:
        manifest = self._submission_manifest(
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
        )
        manifest, secret_payload = _resolve_secret_payload(manifest, None)
        return manifest, secret_payload

    def _submit_argv(
        self,
        *,
        binary: str,
        manifest_path: pathlib.Path,
        wrapper_path: pathlib.Path,
        extra_scripts: Sequence[str],
        secret_payload_path: Optional[pathlib.Path],
        upstream_checkpoint: str,
        dry_run: Optional[str],
        kube_context: Optional[str],
        namespace: Optional[str],
        team: Optional[str],
        preset: Optional[str],
        gpu_class: Optional[str],
        gpu_resource_mode: Optional[str],
        node_selector: Any,
        disable_default_priorities: bool = False,
    ) -> List[str]:
        params = self._params
        _update_generated_run_config(
            manifest_path,
            wrapper_path=wrapper_path,
            workload_kind=EVAL_WORKLOAD_KIND,
            extra_scripts=extra_scripts,
            namespace=namespace or params.namespace,
            team=team or params.team,
            preset=preset or params.preset,
            gpu_class=gpu_class or params.gpu_class,
            gpu_resource_mode=gpu_resource_mode or params.gpu_resource_mode,
            node_selector=node_selector if node_selector is not None else params.node_selector,
            disable_default_priorities=disable_default_priorities,
            secret_payload_path=secret_payload_path,
            upstream_checkpoint=upstream_checkpoint,
        )
        argv: List[str] = [binary, "run", "--config", str(manifest_path)]
        _append_optional_flag(argv, "--dry-run", dry_run)
        _append_optional_flag(argv, "--context", kube_context)
        return argv

    def submit(
        self,
        *,
        upstream_checkpoint: Optional[str] = None,
        tau_binary: Optional[str] = None,
        dry_run: Optional[str] = None,
        kube_context: Optional[str] = None,
        namespace: Optional[str] = None,
        team: Optional[str] = None,
        preset: Optional[str] = None,
        gpu_class: Optional[str] = None,
        gpu_resource_mode: Optional[str] = None,
        node_selector: Any = None,
        data_pvc: Optional[str] = None,
        cpu_request: Optional[int] = None,
        memory_request: Optional[str] = None,
        cpu_limit: Optional[int] = None,
        memory_limit: Optional[str] = None,
        worker_cpu_request: Optional[int] = None,
        worker_memory_request: Optional[str] = None,
        worker_cpu_limit: Optional[int] = None,
        worker_memory_limit: Optional[str] = None,
        disable_default_priorities: bool = False,
        capture: bool = False,
    ) -> subprocess.CompletedProcess:
        """Synthesize an eval manifest, ship it to the tau Go CLI.

        ``upstream_checkpoint`` is the absolute pod-side path to the
        upstream train run's checkpoint file. Required (the eval ctx is
        useless without it). The tau-py orchestrator resolves this from
        the upstream @tau.train handle's output dir; callers shelling
        ``submit()`` directly must pass it explicitly.
        """
        _refuse_cluster_submit()
        _check_dry_run(dry_run)
        if not upstream_checkpoint:
            raise ValueError(
                "tau-py: @tau.eval .submit() requires upstream_checkpoint "
                "(absolute path inside the pod, e.g. "
                "'/data/checkpoints/my-train/<checkpoint_artifact>'). The tau-py "
                "orchestrator (`tau-py submit experiment.py`) sets this "
                "automatically from the upstream @tau.train handle's "
                "declared checkpoint artifact."
            )

        binary = tau_binary or _find_tau_binary()
        manifest, secret_payload = self._submit_manifest(
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
        )
        with tempfile.TemporaryDirectory(prefix="tau-py-eval-") as tmp:
            tmp_path = pathlib.Path(tmp)
            secret_payload_path = _write_job_secret_file(tmp_path, secret_payload)
            manifest_path, wrapper_path, extra_scripts = _write_submission_files(
                tmp_path,
                name=self._params.name,
                manifest=manifest,
                source_path=self._source_path,
            )
            argv = self._submit_argv(
                binary=binary,
                manifest_path=manifest_path,
                wrapper_path=wrapper_path,
                extra_scripts=extra_scripts,
                secret_payload_path=secret_payload_path,
                upstream_checkpoint=upstream_checkpoint,
                dry_run=dry_run,
                kube_context=kube_context,
                namespace=namespace,
                team=team,
                preset=preset,
                gpu_class=gpu_class,
                gpu_resource_mode=gpu_resource_mode,
                node_selector=node_selector,
                disable_default_priorities=disable_default_priorities,
            )

            return subprocess.run(argv, **_completed_process_kwargs(capture))


def eval(
    *,
    name: str,
    after: Optional[str] = None,
    gpus: int = 1,
    cpu_workers: int = 1,
    gpu_class: Optional[str] = None,
    gpu_resource_mode: Optional[str] = None,
    node_selector: Any = None,
    data_pvc: Optional[str] = None,
    cpu_request: Optional[int] = None,
    memory_request: Optional[str] = None,
    cpu_limit: Optional[int] = None,
    memory_limit: Optional[str] = None,
    worker_cpu_request: Optional[int] = None,
    worker_memory_request: Optional[str] = None,
    worker_cpu_limit: Optional[int] = None,
    worker_memory_limit: Optional[str] = None,
    namespace: Optional[str] = None,
    team: Optional[str] = None,
    preset: Optional[str] = None,
    extra_manifest: Optional[Dict[str, Any]] = None,
) -> Callable[[Callable[[Ctx], Any]], _EvalHandle]:
    """Decorator: register a function as a Tau eval entrypoint.

    Eval is a separate workload kind from train: a CPU-only system head,
    1 GPU worker (holds a Ray actor), and ``cpu_workers`` CPU-only worker pods (run ``ray.remote``
    fanout tasks). Use this for the "score N initial conditions against a
    trained checkpoint" pattern where most of the eval work is CPU-bound
    (loading data, post-processing) but you need at least one GPU for
    the model forward::

        @tau.train(name="my-fullft", gpus=8, team="research",
                    checkpoint_artifact="final.safetensors",
                    extra_manifest={"runtime": {"pip": ["torch==2.4.0"]}})
        def train(ctx):
            ...
            save_checkpoint(ctx.checkpoints_dir / "final.safetensors")

        @tau.eval(name="my-fullft-eval", after="my-fullft",
                   gpus=1, cpu_workers=19, team="research")
        def eval(ctx):
            import ray

            @ray.remote(num_gpus=1)
            class Inferencer:
                def __init__(self, ckpt):
                    self.model = load_model(ckpt).cuda().eval()
                def forward(self, x):
                    with torch.no_grad():
                        return self.model(x.cuda()).cpu()

            @ray.remote(num_cpus=1)
            def score_one(actor, ic):
                return compute_tc(ray.get(actor.forward.remote(load_ic(ic))))

            actor = Inferencer.remote(ctx.upstream_checkpoint)
            ics = sorted(ctx.datasets_dir.glob("*.npz"))
            return ray.get([score_one.remote(actor, ic) for ic in ics])

    ``after`` names the upstream ``@tau.train`` handle. The tau-py
    orchestrator (``tau-py submit experiment.py``) discovers both
    handles in the same file, submits train, polls to completion, and
    then submits eval with ``ctx.upstream_checkpoint`` populated from
    the train handle's declared ``checkpoint_artifact`` under the train run's
    durable checkpoint dir. Without an orchestrator, call
    ``handle.submit(upstream_checkpoint=...)`` directly.

    All other cluster-side details (eval Kueue queue, DRA claim, image,
    priority class, tolerations) are inferred by the tau CLI from
    ``team`` + ``gpus`` via the topology preset system (lane=eval).
    """

    def decorator(fn: Callable[[Ctx], Any]) -> _EvalHandle:
        return _EvalHandle(
            fn,
            name=name,
            after=after,
            gpus=gpus,
            cpu_workers=cpu_workers,
            gpu_class=gpu_class,
            gpu_resource_mode=gpu_resource_mode,
            node_selector=node_selector,
            data_pvc=data_pvc,
            cpu_request=cpu_request,
            memory_request=memory_request,
            cpu_limit=cpu_limit,
            memory_limit=memory_limit,
            worker_cpu_request=worker_cpu_request,
            worker_memory_request=worker_memory_request,
            worker_cpu_limit=worker_cpu_limit,
            worker_memory_limit=worker_memory_limit,
            namespace=namespace,
            team=team,
            preset=preset,
            extra_manifest=extra_manifest,
        )

    return decorator


def _find_tau_binary() -> str:
    """Locate the tau Go CLI on PATH; honor TAU_BINARY env override."""
    override = os.environ.get("TAU_BINARY")
    if override:
        if not pathlib.Path(override).exists():
            raise RuntimeError("TAU_BINARY=" + override + " does not exist")
        return override
    found = shutil.which("tau")
    if not found:
        raise RuntimeError(
            "tau-py: cannot find the `tau` CLI on PATH. Install the Tau CLI "
            "separately or set TAU_BINARY=/path/to/tau."
        )
    return found


def _binary_has_experiment_verb(binary: str) -> bool:
    """Report whether ``binary`` still exposes the ``experiment`` command group."""
    try:
        completed = subprocess.run(
            [binary, "experiment", "--help"],
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return completed.returncode == 0


def _find_portal_binary() -> str:
    """Locate the taugrid-portal CLI, which owns the Stellar experiment store.

    Stellar and Portal moved out of the `tau` binary into `taugrid-portal`, so
    `experiment ...` verbs live there now. A `tau` from before that split still
    understands them, so fall back to it when it does — but never silently hand
    work to a post-split `tau`, which would fail with an opaque "unknown
    command" error far from its cause.
    """
    override = os.environ.get("TAUGRID_PORTAL_BINARY")
    if override:
        if not pathlib.Path(override).exists():
            raise RuntimeError("TAUGRID_PORTAL_BINARY=" + override + " does not exist")
        return override
    found = shutil.which("taugrid-portal")
    if found:
        return found
    legacy = os.environ.get("TAU_BINARY") or shutil.which("tau")
    if legacy and _binary_has_experiment_verb(legacy):
        warnings.warn(
            "tau-py: falling back to the `tau` CLI for Stellar sync. Stellar now "
            "ships as `taugrid-portal`; install it (make install-taugrid-portal) "
            "or set TAUGRID_PORTAL_BINARY=/path/to/taugrid-portal.",
            DeprecationWarning,
            stacklevel=2,
        )
        return legacy
    raise RuntimeError(
        "tau-py: cannot find the `taugrid-portal` CLI on PATH, and the `tau` on "
        "PATH does not provide `experiment` commands. Stellar moved out of the "
        "tau binary: install taugrid-portal (make install-taugrid-portal) or set "
        "TAUGRID_PORTAL_BINARY=/path/to/taugrid-portal."
    )
