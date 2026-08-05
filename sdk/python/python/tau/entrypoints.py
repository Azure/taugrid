"""Helpers for loading staged Python entrypoints inside Tau jobs."""

from __future__ import annotations

import hashlib
import importlib.util
import sys
from collections.abc import Callable
from pathlib import Path
from types import ModuleType
from typing import Any


_RESERVED_MODULE_NAMES = frozenset({"tau", "config", "workloads"})


def load_staged_module(script_path: str | Path, *, module_name: str | None = None) -> ModuleType:
    """Import a Python script by path while temporarily enabling sibling imports.

    ``script_path`` should usually be an absolute PVC path when called from a
    Tau job. The script's directory is prepended to ``sys.path`` only while the
    module executes, then ``sys.path`` is restored.
    """

    path = Path(script_path).expanduser().resolve(strict=False)
    if not path.is_file():
        raise FileNotFoundError(f"tau staged module not found: {path}")

    name = module_name or _module_name_for_path(path)
    _validate_module_name(name, path)

    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise ImportError(f"could not import staged module from {path}")

    module = importlib.util.module_from_spec(spec)
    original_sys_path = list(sys.path)
    sys.modules[name] = module
    try:
        parent = str(path.parent)
        if parent not in sys.path:
            sys.path.insert(0, parent)
        spec.loader.exec_module(module)
    except Exception:
        if sys.modules.get(name) is module:
            sys.modules.pop(name, None)
        raise
    finally:
        sys.path[:] = original_sys_path
    return module


def load_staged_function(
    script_path: str | Path,
    function_name: str,
    *,
    module_name: str | None = None,
) -> Callable[..., Any]:
    """Load a callable from a staged Python script."""

    if not function_name:
        raise ValueError("function_name must not be empty")
    module = load_staged_module(script_path, module_name=module_name)
    try:
        fn = getattr(module, function_name)
    except AttributeError as exc:
        raise AttributeError(f"tau staged module {Path(script_path)} has no attribute {function_name!r}") from exc
    if not callable(fn):
        raise TypeError(f"tau staged module {Path(script_path)} attribute {function_name!r} is not callable")
    return fn


def call_staged_function(
    script_path: str | Path,
    function_name: str,
    *args: Any,
    module_name: str | None = None,
    **kwargs: Any,
) -> Any:
    """Load and call a function from a staged Python script."""

    fn = load_staged_function(script_path, function_name, module_name=module_name)
    return fn(*args, **kwargs)


def _module_name_for_path(path: Path) -> str:
    stem = path.stem
    if not stem.isidentifier():
        raise ValueError(f"staged module filename must be an importable Python module name: {path.name!r}")
    digest = hashlib.sha1(str(path).encode("utf-8")).hexdigest()[:12]
    return f"_tau_staged_{stem}_{digest}"


def _validate_module_name(name: str, path: Path) -> None:
    if not name.isidentifier():
        raise ValueError(f"staged module name must be a Python identifier for {path}: {name!r}")
    if name in _RESERVED_MODULE_NAMES:
        raise ValueError(f"staged module name {name!r} is reserved by Tau")


__all__ = ["call_staged_function", "load_staged_function", "load_staged_module"]
