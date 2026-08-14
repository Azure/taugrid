# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Python wrapper for ``tau serve deploy``."""

from __future__ import annotations

import shlex
import subprocess
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

from tau.config import runtime_env_entries
from tau.workloads import (
    _append_optional_flag,
    _check_dry_run,
    _completed_process_kwargs,
    _find_tau_binary,
    _refuse_cluster_submit,
)


@dataclass(frozen=True)
class ServeHandle:
    """Deployable Tau serving endpoint."""

    _tau_serve_handle = True

    name: str
    profile: str
    from_finetune: Optional[Any] = None
    checkpoint_ref: Optional[str] = None
    from_model: Optional[str] = None
    model_ref: Optional[str] = None
    checkpoint: Optional[str] = None
    checkpoint_pvc: str = "blob-training"
    namespace: Optional[str] = None
    kind: str = "rayservice"
    image: Optional[str] = None
    replicas: Optional[int] = None
    import_path: Optional[str] = None
    env: Mapping[str, Any] | Sequence[str] | None = None
    runtime_pip: tuple[str, ...] = field(default_factory=tuple)
    args: str | Sequence[str] | None = None
    profiles_dir: tuple[str | Path, ...] = field(default_factory=tuple)
    port: Optional[int] = None
    volumes: tuple[str, ...] = field(default_factory=tuple)
    mounts: tuple[str, ...] = field(default_factory=tuple)

    def __post_init__(self) -> None:
        if not self.name:
            raise ValueError("tau-py: tau.serve(name=...) is required")
        if not self.profile:
            raise ValueError("tau-py: tau.serve(profile=...) is required")
        if self.kind not in ("rayservice", "deployment"):
            raise ValueError("tau-py: tau.serve(kind=...) must be one of: rayservice, deployment")
        if self.replicas is not None and self.replicas < 0:
            raise ValueError("tau-py: tau.serve(replicas=...) must be >= 0")
        if self.port is not None and self.port <= 0:
            raise ValueError("tau-py: tau.serve(port=...) must be positive")
        if self.kind == "deployment" and self.runtime_pip:
            raise ValueError("tau-py: runtime_pip is only supported for kind='rayservice'")
        _validate_checkpoint_source_count(
            _resolve_finetune_ref(self.from_finetune),
            self.checkpoint_ref,
            self.from_model,
            self.model_ref,
            self.checkpoint,
        )

    def submit(
        self,
        *,
        tau_binary: Optional[str] = None,
        dry_run: Optional[str] = None,
        kube_context: Optional[str] = None,
        profiles_dir: Optional[str | Path | Sequence[str | Path]] = None,
        extra_args: Optional[Sequence[str]] = None,
        capture: bool = False,
    ) -> subprocess.CompletedProcess:
        """Deploy with ``tau serve deploy``."""
        return self._run(
            tau_binary=tau_binary,
            dry_run=dry_run,
            kube_context=kube_context,
            profiles_dir=profiles_dir,
            extra_args=extra_args,
            capture=capture,
        )

    def deploy(
        self,
        *,
        tau_binary: Optional[str] = None,
        namespace: Optional[str] = None,
        dry_run: Optional[str] = None,
        kube_context: Optional[str] = None,
        profiles_dir: Optional[str | Path | Sequence[str | Path]] = None,
        capture: bool = False,
        extra_args: Optional[Sequence[str]] = None,
    ) -> subprocess.CompletedProcess:
        """Alias for :meth:`submit`, matching ``tau serve deploy`` wording."""
        return self._run(
            tau_binary=tau_binary,
            namespace=namespace,
            dry_run=dry_run,
            kube_context=kube_context,
            profiles_dir=profiles_dir,
            extra_args=extra_args,
            capture=capture,
        )

    def _run(
        self,
        *,
        tau_binary: Optional[str],
        namespace: Optional[str] = None,
        dry_run: Optional[str],
        kube_context: Optional[str],
        profiles_dir: Optional[str | Path | Sequence[str | Path]],
        extra_args: Optional[Sequence[str]],
        capture: bool,
    ) -> subprocess.CompletedProcess:
        _refuse_cluster_submit("deploy")
        argv = self._deploy_argv(
            tau_binary=tau_binary,
            namespace=namespace,
            dry_run=dry_run,
            kube_context=kube_context,
            profiles_dir=profiles_dir,
            extra_args=extra_args,
        )
        return subprocess.run(argv, **_completed_process_kwargs(capture))

    def _deploy_argv(
        self,
        *,
        tau_binary: Optional[str] = None,
        namespace: Optional[str] = None,
        dry_run: Optional[str] = None,
        kube_context: Optional[str] = None,
        profiles_dir: Optional[str | Path | Sequence[str | Path]] = None,
        extra_args: Optional[Sequence[str]] = None,
    ) -> list[str]:
        _check_dry_run(dry_run)

        binary = tau_binary or _find_tau_binary()
        argv = [
            binary,
            "serve",
            "deploy",
            self.name,
            "--kind",
            self.kind,
            "--profile",
            self.profile,
        ]

        finetune_ref = _resolve_finetune_ref(self.from_finetune)
        if finetune_ref:
            argv += ["--from-finetune", finetune_ref]
        if self.checkpoint_ref:
            argv += ["--checkpoint-ref", self.checkpoint_ref]
        if self.from_model:
            argv += ["--from-model", self.from_model]
        if self.model_ref:
            argv += ["--model-ref", self.model_ref]
        if self.checkpoint:
            argv += ["--checkpoint", self.checkpoint]
        if self.checkpoint_pvc != "blob-training" and (
            finetune_ref or self.checkpoint_ref or self.from_model or self.model_ref or self.checkpoint
        ):
            argv += ["--checkpoint-pvc", self.checkpoint_pvc]
        if self.image:
            argv += ["--image", self.image]
        if self.replicas is not None:
            argv += ["--replicas", str(int(self.replicas))]
        if self.import_path:
            argv += ["--import-path", self.import_path]
        if self.port is not None:
            argv += ["--port", str(self.port)]
        for profile_dir in self.profiles_dir:
            argv += ["--profiles-dir", str(profile_dir)]
        for profile_dir in _profiles_dirs(profiles_dir):
            argv += ["--profiles-dir", str(profile_dir)]
        for volume in self.volumes:
            argv += ["--volume", volume]
        for mount in self.mounts:
            argv += ["--mount", mount]
        for flag, env_item in _env_cli_flags(self.env):
            argv += [flag, env_item]
        for pkg in self.runtime_pip:
            argv += ["--runtime-pip", pkg]
        if self.args:
            argv += ["--args", _args_string(self.args)]

        _append_optional_flag(argv, "-n", namespace or self.namespace)
        _append_optional_flag(argv, "--dry-run", dry_run)
        _append_optional_flag(argv, "--context", kube_context)
        if extra_args:
            argv += list(extra_args)
        return argv


def serve(
    *,
    name: str,
    profile: str,
    from_finetune: Optional[Any] = None,
    checkpoint_ref: Optional[str] = None,
    from_model: Optional[str] = None,
    model_ref: Optional[str] = None,
    checkpoint: Optional[str] = None,
    checkpoint_pvc: str = "blob-training",
    namespace: Optional[str] = None,
    kind: str = "rayservice",
    image: Optional[str] = None,
    replicas: Optional[int] = None,
    import_path: Optional[str] = None,
    env: Mapping[str, Any] | Sequence[str] | None = None,
    runtime_pip: Optional[Sequence[str]] = None,
    args: str | Sequence[str] | None = None,
    profiles_dir: str | Path | Sequence[str | Path] | None = None,
    port: Optional[int] = None,
    volumes: Optional[Sequence[str]] = None,
    mounts: Optional[Sequence[str]] = None,
) -> ServeHandle:
    """Describe a Tau serving endpoint.

    The returned handle exposes both ``.submit(...)`` and ``.deploy(...)``.
    """
    return ServeHandle(
        name=name,
        profile=profile,
        from_finetune=from_finetune,
        checkpoint_ref=checkpoint_ref,
        from_model=from_model,
        model_ref=model_ref,
        checkpoint=checkpoint,
        checkpoint_pvc=checkpoint_pvc,
        namespace=namespace,
        kind=kind,
        image=image,
        replicas=replicas,
        import_path=import_path,
        env=env,
        runtime_pip=tuple(str(pkg) for pkg in (runtime_pip or ())),
        args=args,
        profiles_dir=_profiles_dirs(profiles_dir),
        port=port,
        volumes=tuple(volumes or ()),
        mounts=tuple(mounts or ()),
    )


def _validate_checkpoint_source_count(*values: Optional[str]) -> None:
    if sum(bool(v) for v in values) > 1:
        raise ValueError(
            "tau-py: tau.serve accepts at most one of from_finetune, "
            "checkpoint_ref, from_model, model_ref, or checkpoint"
        )


def _resolve_finetune_ref(value: Optional[Any]) -> Optional[str]:
    if value is None:
        return None
    if isinstance(value, str):
        if not value:
            raise ValueError("tau-py: from_finetune cannot be empty")
        return value
    if getattr(value, "_tau_train_handle", False):
        params = getattr(value, "_params", {})
        name = getattr(params, "name", None)
        if name:
            return str(name)
    raise ValueError(
        "tau-py: from_finetune must be a train handle returned by @tau.train "
        "or a finetune run name"
    )


def _env_cli_flags(env: Mapping[str, Any] | Sequence[str] | None) -> list[tuple[str, str]]:
    out: list[tuple[str, str]] = []
    for entry in runtime_env_entries(env):
        name = str(entry.get("name") or "")
        if not name or "=" in name:
            raise ValueError(f"tau.serve: invalid env key {name!r}")
        secret = ((entry.get("valueFrom") or {}).get("secretKeyRef") or {})
        if secret:
            secret_name = str(secret.get("name") or "")
            secret_key = str(secret.get("key") or "")
            if not secret_name or not secret_key:
                raise ValueError(f"tau.serve: invalid secret env entry for {name!r}")
            out.append(("--env-secret", f"{name}={secret_name}:{secret_key}"))
        else:
            out.append(("--env", f"{name}={entry.get('value', '')}"))
    return out


def _profiles_dirs(value: str | Path | Sequence[str | Path] | None) -> tuple[str | Path, ...]:
    if value is None:
        return ()
    if isinstance(value, (str, Path)):
        return (value,)
    return tuple(value)


def _args_string(args: str | Sequence[str]) -> str:
    if isinstance(args, str):
        return args
    return shlex.join(str(part) for part in args)
