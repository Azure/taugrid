"""Lightweight project-owned config helpers for tau-py."""

from __future__ import annotations

import copy
import os
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml

try:  # Python 3.11+
    import tomllib  # type: ignore[import-not-found]
except ModuleNotFoundError:  # pragma: no cover - exercised on Python 3.10
    import tomli as tomllib  # type: ignore[no-redef]


@dataclass(frozen=True)
class SecretRef:
    """Reference a Kubernetes Secret key without embedding its value."""

    name: str
    key: str

    def __post_init__(self) -> None:
        if not self.name or not self.key:
            raise ValueError("tau.secret_ref requires non-empty secret name and key")


def secret_ref(name: str, key: str) -> SecretRef:
    return SecretRef(name=name, key=key)


def secret(name: str, key: str) -> SecretRef:
    return secret_ref(name, key)


@dataclass(frozen=True)
class SecretSource:
    """Source a job-scoped Kubernetes Secret key from local state."""

    key: str
    env: str | None = None
    path: str | None = None

    def __post_init__(self) -> None:
        if not self.key:
            raise ValueError("tau secret sources require a non-empty key")
        if bool(self.env) == bool(self.path):
            raise ValueError("tau secret sources require exactly one of env or path")


def secret_from_env(key: str, *, env: str | None = None) -> SecretSource:
    return SecretSource(key=key, env=env or key)


def secret_from_file(key: str, *, path: str) -> SecretSource:
    return SecretSource(key=key, path=path)


def pvc_mount(name: str, *, pvc: str, mount_path: str, read_only: bool = False) -> dict[str, Any]:
    if not name or not pvc or not mount_path.startswith("/"):
        raise ValueError("tau.pvc_mount requires name, pvc, and an absolute mount_path")
    return {"name": name, "pvc": pvc, "mountPath": mount_path, "readOnly": bool(read_only)}


def load_config(source: str | Path | Mapping[str, Any] | Sequence[Any] | None) -> dict[str, Any]:
    """Load and deep-merge YAML/TOML/dict config sources.

    Lists of sources are merged left-to-right. Later sources override earlier
    mappings, matching "config file as source of truth, env/CLI as overrides".
    """

    if source is None:
        return {}
    if isinstance(source, Mapping):
        return copy.deepcopy(dict(source))
    if isinstance(source, (str, Path)):
        path = Path(source)
        if not path.exists():
            raise FileNotFoundError(f"tau config not found: {path}")
        suffix = path.suffix.lower()
        if suffix in (".yaml", ".yml"):
            data = yaml.safe_load(path.read_text()) or {}
        elif suffix == ".toml":
            data = tomllib.loads(path.read_text())
        else:
            raise ValueError(f"tau config {path} must be .yaml, .yml, or .toml")
        if not isinstance(data, Mapping):
            raise ValueError(f"tau config {path} must contain a mapping")
        return copy.deepcopy(dict(data))
    if isinstance(source, Sequence) and not isinstance(source, (str, bytes, bytearray)):
        merged: dict[str, Any] = {}
        for item in source:
            merged = deep_merge(merged, load_config(item))
        return merged
    raise TypeError(f"unsupported tau config source: {type(source)!r}")


def config(
    *sources: str | Path | Mapping[str, Any],
    overrides: Mapping[str, Any] | None = None,
    env: Mapping[str, str] | None = None,
) -> dict[str, Any]:
    merged = load_config(list(sources))
    if env:
        env_overrides: dict[str, Any] = {}
        for dest_path, env_name in env.items():
            if env_name in os.environ:
                _set_path(env_overrides, dest_path, os.environ[env_name])
        merged = deep_merge(merged, env_overrides)
    if overrides:
        merged = deep_merge(merged, dict(overrides))
    return merged


def deep_merge(base: Mapping[str, Any], override: Mapping[str, Any]) -> dict[str, Any]:
    out = copy.deepcopy(dict(base))
    for key, value in override.items():
        if isinstance(value, Mapping) and isinstance(out.get(key), Mapping):
            out[key] = deep_merge(out[key], value)  # type: ignore[arg-type]
        else:
            out[key] = copy.deepcopy(value)
    return out


def runtime_env_entries(env: Mapping[str, Any] | Sequence[Any] | None) -> list[dict[str, Any]]:
    if env is None:
        return []
    if isinstance(env, Mapping):
        return [_env_entry(str(key), value) for key, value in env.items()]
    if isinstance(env, (str, bytes, bytearray)):
        raise ValueError("tau env must be a mapping or sequence of env entries")
    out: list[dict[str, Any]] = []
    for item in env:
        if isinstance(item, Mapping):
            out.append(copy.deepcopy(dict(item)))
            continue
        item_str = str(item)
        if "=" not in item_str or item_str.startswith("="):
            raise ValueError(f"tau env entries must be KEY=VALUE, got {item_str!r}")
        key, value = item_str.split("=", 1)
        out.append(_env_entry(key, value))
    return out


def merge_runtime_env(existing: Any, extra: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    entries = runtime_env_entries(existing)
    by_name = {str(item.get("name")): dict(item) for item in entries if item.get("name")}
    for item in extra:
        name = str(item.get("name") or "")
        if not name:
            raise ValueError("tau env entry missing name")
        by_name[name] = copy.deepcopy(dict(item))
    return [by_name[key] for key in sorted(by_name)]


def normalize_mounts(mounts: Sequence[Any] | None) -> list[dict[str, Any]]:
    if not mounts:
        return []
    out = []
    for item in mounts:
        if not isinstance(item, Mapping):
            raise ValueError("tau mounts must be mappings; use tau.pvc_mount(...)")
        out.append(copy.deepcopy(dict(item)))
    return out


def _env_entry(name: str, value: Any) -> dict[str, Any]:
    if not name or "=" in name:
        raise ValueError(f"invalid env var name {name!r}")
    if isinstance(value, SecretSource):
        source: dict[str, str] = {"key": value.key}
        if value.env:
            source["env"] = value.env
        if value.path:
            source["path"] = value.path
        return {
            "name": name,
            "valueFrom": {
                "tauSecretSource": source,
            },
        }
    if isinstance(value, SecretRef):
        return {
            "name": name,
            "valueFrom": {
                "secretKeyRef": {
                    "name": value.name,
                    "key": value.key,
                }
            },
        }
    return {"name": name, "value": str(value)}


def _set_path(target: dict[str, Any], dotted_path: str, value: Any) -> None:
    parts = [part for part in dotted_path.split(".") if part]
    if not parts:
        raise ValueError("env override path must not be empty")
    cur = target
    for part in parts[:-1]:
        child = cur.get(part)
        if not isinstance(child, dict):
            child = {}
            cur[part] = child
        cur = child
    cur[parts[-1]] = value
