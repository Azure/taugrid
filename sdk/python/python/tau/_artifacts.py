"""Shared artifact validation helpers for tau-py internals."""

from __future__ import annotations

from pathlib import PurePosixPath

DEFAULT_CHECKPOINT_ARTIFACT = "last.safetensors"


def validate_checkpoint_artifact(
    path: object,
    *,
    subject: str,
    error_type: type[Exception],
) -> str:
    if not isinstance(path, str):
        raise error_type(f"{subject} must be a string")
    value = path.strip()
    if not value:
        raise error_type(f"{subject} must not be empty")
    p = PurePosixPath(value)
    if p.is_absolute() or any(part in ("", ".", "..") for part in p.parts):
        raise error_type(
            f"{subject} must be a relative pod path without "
            f"'.' or '..' segments (got {path!r})"
        )
    return p.as_posix()
