# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Render a staged notebook payload into a Kueue-admitted ray.io/v1 RayJob.

This is a constrained port of the Go CLI's rayjobrender, matching the CLI's
RayJob contract for the supported notebook shape and refusing anything it cannot
represent faithfully.

Contracts mirrored from the CLI:

- spec.suspend: true, spec.submissionMode: HTTPMode, and a 15-second
  ttlSecondsAfterFinished (cli/internal/rayjobrender/render.go:63, :471-482);
- the Kueue queue label on the workload metadata;
- a rayVersion matching the image, a control-only Ray head (num-cpus/num-gpus
  "0") whose Kubernetes requests/limits are still 2 CPU/8Gi -> 4 CPU/16Gi
  (cli/internal/rayjobrender/render.go:845);
- the embedded payload: a tau-payload initContainer decodes the gzip+base64
  envelope from TAU_PAYLOAD_B64 into a shared /script emptyDir, exactly like the
  CLI, so no object-store upload, ConfigMap, or PVC is needed;
- the CLI's data (/data), hot (/mnt) and dshm (/dev/shm) volumes on every pod;
- literal env and SecretKeyRef env on both head and workers;
- a pod-level nodeSelector and priorityClassName;
- runtimeEnvYAML.pip plus the pip-install entrypoint preamble.

Refusal is first-class: unsupported shapes raise UnsupportedShape with a clear
message instead of silently rendering something different.
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass
from typing import Any, Dict, List, Mapping, Optional

import yaml

from tau._payload import (
    ANNOTATION_DIGEST,
    ENV_B64,
    ENV_DIGEST,
    ENV_TARGET_DIR,
    INIT_CONTAINER_NAME,
    INIT_CONTAINER_SCRIPT,
    TARGET_DIR,
    VOLUME_NAME,
    EncodedPayload,
)

TAU_QUEUE_LABEL = "kueue.x-k8s.io/queue-name"
#: Mirrors core/workloadmeta: the CLI stamps these so a rendered workload is
#: identifiable as Tau-managed (and therefore discoverable by list commands).
TAU_MANAGED_BY_LABEL = "tau.azure.com/managed-by"
TAU_MANAGED_BY_VALUE = "tau"
DEFAULT_IMAGE = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0"
#: The certified runtime image is an operator decision, so it is overridable.
#: It must ship ray plus the notebook executor (nbformat/nbconvert/ipykernel).
RUNTIME_IMAGE_ENV = "TAUGRID_RUNTIME_IMAGE"


def default_image() -> str:
    """The runtime image, overridable by the operator via TAUGRID_RUNTIME_IMAGE."""
    import os

    return os.environ.get(RUNTIME_IMAGE_ENV, "").strip() or DEFAULT_IMAGE

#: Mirrors the CLI constants exactly.
RAY_JOB_TTL_SECONDS = 15
SUBMISSION_MODE = "HTTPMode"
DURABLE_ROOT = "/data"
HOT_ROOT = "/mnt"
DSHM_MOUNT = "/dev/shm"
DSHM_SIZE = "16Gi"

HEAD_REQUESTS = {"cpu": "2", "memory": "8Gi"}
HEAD_LIMITS = {"cpu": "4", "memory": "16Gi"}

_SECRET_REF_RE = re.compile(r"^[^\s:]+:[^\s:]+$")
_RAY_VERSION_RE = re.compile(r"-ray((\d+)\.(\d+)\.\d+)")
_SUPPORTED_RUNTIME_KEYS = {"pip"}


class UnsupportedShape(ValueError):
    """The requested shape is not representable by the constrained renderer."""


@dataclass(frozen=True)
class Profile:
    """A resolved worker profile (subset of a TauCluster workloadProfile)."""

    name: str
    workers: int = 1
    gpus_per_worker: int = 0
    num_cpus_per_worker: Optional[float] = None
    priority_class: Optional[str] = None
    node_selector: Optional[Dict[str, str]] = None
    image: Optional[str] = None
    memory_per_worker: Optional[str] = None

    def __post_init__(self):
        if type(self.workers) is not int or self.workers < 1 or type(self.gpus_per_worker) is not int or self.gpus_per_worker < 0:
            raise UnsupportedShape("workerCount must be a positive integer and gpusPerWorker a nonnegative integer")
        cpu = self.num_cpus_per_worker
        if cpu is not None and (type(cpu) not in (int, float) or not math.isfinite(cpu) or cpu <= 0):
            raise UnsupportedShape("cpusPerWorker must be a positive finite number")
        if self.node_selector is not None and (not isinstance(self.node_selector, dict) or not all(isinstance(key, str) and isinstance(value, str) for key, value in self.node_selector.items())):
            raise UnsupportedShape("nodeSelector must map strings to strings")


def refuse(*, archives: bool = False, chained: bool = False, offload: bool = False, profiler: bool = False) -> None:
    """Fail loudly for shapes the constrained renderer cannot represent."""
    if archives:
        raise UnsupportedShape(
            "project archives / multi-file projects are not supported by the "
            "constrained renderer; submit a single .ipynb"
        )
    if chained:
        raise UnsupportedShape(
            "managed train/eval chaining is not supported by the notebook "
            "widget; submit one run at a time"
        )
    if offload:
        raise UnsupportedShape(
            "artifact offload configuration is not implemented by the notebook renderer"
        )
    if profiler:
        raise UnsupportedShape("profiler environment is not supported by the notebook renderer")


def render_rayjob(
    *,
    name: str,
    namespace: str,
    queue: str,
    profile: Profile,
    entrypoint: str,
    runtime_env: Optional[Mapping[str, Any]] = None,
    env: Optional[Mapping[str, str]] = None,
    env_secret: Optional[Mapping[str, str]] = None,
    payload: Optional[EncodedPayload] = None,
    ttl_seconds: int = RAY_JOB_TTL_SECONDS,
    submission_mode: str = SUBMISSION_MODE,
) -> Dict[str, Any]:
    """Render a Kueue-admitted ray.io/v1 RayJob executing entrypoint.

    entrypoint is the base command that runs the notebook (typically
    python3 /script/_tau_runner.py --notebook analysis.ipynb). When payload is
    set it is embedded and the initContainer materialises it at /script; pip
    declarations add the CLI install preamble and runtimeEnvYAML.
    """
    _validate_runtime_env(runtime_env)
    env_vars = _env_list(env, env_secret)
    pip = list((runtime_env or {}).get("pip") or [])
    image = profile.image or default_image()
    num_gpus = str(profile.gpus_per_worker or 0)

    head_pod: Dict[str, Any] = {
        "containers": [_container("ray-head", image, HEAD_REQUESTS, HEAD_LIMITS, env_vars)]
    }
    worker_pod: Dict[str, Any] = {
        "containers": [
            _container("ray-worker", image, _worker_resources(profile), _worker_resources(profile), env_vars)
        ]
    }

    if payload is not None:
        head_pod["initContainers"] = [_payload_init_container(image, payload)]
        head_pod["volumes"] = [_script_volume()]
        head_pod["containers"][0].setdefault("volumeMounts", []).append(
            {"name": VOLUME_NAME, "mountPath": TARGET_DIR, "readOnly": True}
        )

    for pod in (head_pod, worker_pod):
        pod.setdefault("volumes", []).extend(_common_volumes())
        pod["containers"][0].setdefault("volumeMounts", []).extend(_common_mounts())
        if profile.node_selector:
            pod["nodeSelector"] = dict(profile.node_selector)
        if profile.priority_class:
            pod["priorityClassName"] = profile.priority_class

    metadata: Dict[str, Any] = {
        "name": name,
        "namespace": namespace,
        "labels": {TAU_QUEUE_LABEL: queue, TAU_MANAGED_BY_LABEL: TAU_MANAGED_BY_VALUE},
    }
    if payload is not None:
        metadata["annotations"] = {ANNOTATION_DIGEST: payload.digest}

    spec: Dict[str, Any] = {
        "suspend": True,
        "shutdownAfterJobFinishes": True,
        "ttlSecondsAfterFinished": int(ttl_seconds),
        "submissionMode": submission_mode,
        "entrypoint": _entrypoint(entrypoint, pip),
        "rayClusterSpec": {
            "rayVersion": _ray_version(image),
            "headGroupSpec": {
                "rayStartParams": {"num-gpus": "0", "num-cpus": "0"},
                "template": {"spec": head_pod},
            },
            "workerGroupSpecs": [
                {
                    "replicas": profile.workers,
                    "groupName": "notebook-workers",
                    "rayStartParams": {"num-gpus": num_gpus},
                    "template": {"spec": worker_pod},
                }
            ],
        },
    }
    if submission_mode not in ("HTTPMode", "K8sJobMode"):
        raise UnsupportedShape("submission mode must be HTTPMode or K8sJobMode")
    if submission_mode == "K8sJobMode":
        # K8sJobMode submits to Ray and forwards the remote driver output.
        # Preserve the verified notebook runtime and mount contract on this pod;
        # the driver itself executes on the head, not in the submitter.
        submitter_pod: Dict[str, Any] = {
            "restartPolicy": "Never",
            "containers": [{"name": "submitter", "image": image,
                            "resources": {"requests": {"cpu": "100m", "memory": "256Mi"},
                                          "limits": {"cpu": "1", "memory": "1Gi"}}}],
        }
        if payload is not None:
            submitter_pod["initContainers"] = [_payload_init_container(image, payload)]
            submitter_pod["volumes"] = [_script_volume()]
            submitter_pod["containers"][0]["volumeMounts"] = [
                {"name": VOLUME_NAME, "mountPath": TARGET_DIR, "readOnly": True}
            ]
        submitter_pod.setdefault("volumes", []).extend(_common_volumes())
        submitter_pod["containers"][0].setdefault("volumeMounts", []).extend(_common_mounts())
        if profile.node_selector:
            submitter_pod["nodeSelector"] = dict(profile.node_selector)
        if profile.priority_class:
            submitter_pod["priorityClassName"] = profile.priority_class
        spec["submitterPodTemplate"] = {"spec": submitter_pod}
    if pip:
        spec["runtimeEnvYAML"] = _runtime_env_yaml(pip)

    return {"apiVersion": "ray.io/v1", "kind": "RayJob", "metadata": metadata, "spec": spec}


def _validate_runtime_env(runtime_env: Optional[Mapping[str, Any]]) -> None:
    if not runtime_env:
        return
    unknown = sorted(set(runtime_env) - _SUPPORTED_RUNTIME_KEYS)
    if unknown:
        raise UnsupportedShape(
            f"unsupported runtime_env key(s): {', '.join(unknown)}; the notebook renderer "
            f"supports only {', '.join(sorted(_SUPPORTED_RUNTIME_KEYS))}"
        )


def _env_list(
    env: Optional[Mapping[str, str]], env_secret: Optional[Mapping[str, str]]
) -> List[Dict[str, Any]]:
    """Build the container env list, mirroring the CLI's SECRET:KEY contract."""
    literal = dict(env or {})
    secret = dict(env_secret or {})
    overlap = sorted(set(literal) & set(secret))
    if overlap:
        raise UnsupportedShape(f"env key(s) declared as both literal and secret: {', '.join(overlap)}")
    out: List[Dict[str, Any]] = []
    for key in sorted(literal):
        out.append({"name": key, "value": str(literal[key])})
    for key in sorted(secret):
        spec = str(secret[key])
        if not _SECRET_REF_RE.match(spec):
            raise UnsupportedShape(f"env_secret[{key}] must use the SECRET:KEY form, got {spec!r}")
        secret_name, _, secret_key = spec.partition(":")
        out.append(
            {"name": key, "valueFrom": {"secretKeyRef": {"name": secret_name, "key": secret_key}}}
        )
    return out


def _container(
    name: str,
    image: str,
    requests: Dict[str, str],
    limits: Dict[str, str],
    env_vars: List[Dict[str, Any]],
) -> Dict[str, Any]:
    container: Dict[str, Any] = {
        "name": name,
        "image": image,
        "resources": {"requests": dict(requests), "limits": dict(limits)},
    }
    if env_vars:
        container["env"] = [dict(item) for item in env_vars]
    return container


def _worker_resources(profile: Profile) -> Dict[str, str]:
    resources: Dict[str, str] = {
        "cpu": str(profile.num_cpus_per_worker or 4),
        "memory": profile.memory_per_worker or "8Gi",
    }
    if profile.gpus_per_worker > 0:
        resources["nvidia.com/gpu"] = str(profile.gpus_per_worker)
    return resources


def _script_volume() -> Dict[str, Any]:
    return {"name": VOLUME_NAME, "emptyDir": {}}


def _common_volumes() -> List[Dict[str, Any]]:
    return [
        {"name": "data", "emptyDir": {}},
        {"name": "tau-hot", "emptyDir": {}},
        {"name": "dshm", "emptyDir": {"medium": "Memory", "sizeLimit": DSHM_SIZE}},
    ]


def _common_mounts() -> List[Dict[str, Any]]:
    return [
        {"name": "data", "mountPath": DURABLE_ROOT},
        {"name": "tau-hot", "mountPath": HOT_ROOT},
        {"name": "dshm", "mountPath": DSHM_MOUNT},
    ]


def _payload_init_container(image: str, payload: EncodedPayload) -> Dict[str, Any]:
    """Mirror the CLI's tau-payload initContainer (same name, env, mount)."""
    return {
        "name": INIT_CONTAINER_NAME,
        "image": image,
        "command": ["python3", "-c", INIT_CONTAINER_SCRIPT],
        "env": [
            {"name": ENV_B64, "value": payload.encoded},
            {"name": ENV_DIGEST, "value": payload.digest},
            {"name": ENV_TARGET_DIR, "value": TARGET_DIR},
        ],
        "volumeMounts": [{"name": VOLUME_NAME, "mountPath": TARGET_DIR}],
        "resources": {
            "requests": {"cpu": "10m", "memory": "32Mi"},
            "limits": {"cpu": "250m", "memory": "128Mi"},
        },
    }


def _entrypoint(base: str, pip: List[str]) -> str:
    """Prepend the CLI's pip preamble to the base notebook command."""
    lines = ["set -eu"]
    lines.append(
        "if [ -s /script/requirements.txt ]; then "
        "python3 -m pip install --quiet --no-cache-dir -r /script/requirements.txt || "
        "python3 -m pip install --quiet --no-cache-dir --user -r /script/requirements.txt; fi"
    )
    lines.append('PATH="$(python3 -m site --user-base)/bin:$PATH"; export PATH')
    if pip:
        quoted = " ".join(_shell_quote(pkg) for pkg in pip)
        lines.append(
            f"python3 -m pip install --quiet --no-cache-dir {quoted} || "
            f"python3 -m pip install --quiet --no-cache-dir --user {quoted}"
        )
    lines.append("cd " + DURABLE_ROOT)
    lines.append(base)
    return "\n".join(lines) + "\n"


def _runtime_env_yaml(pip: List[str]) -> str:
    return yaml.safe_dump({"pip": list(pip)}, sort_keys=False)


def _ray_version(image: str) -> str:
    match = _RAY_VERSION_RE.search(image)
    return match.group(1) if match else "2.56.0"


def _shell_quote(value: str) -> str:
    return "'" + str(value).replace("'", "'\\''") + "'"


__all__ = [
    "UnsupportedShape",
    "Profile",
    "render_rayjob",
    "refuse",
    "TAU_QUEUE_LABEL",
    "default_image",
    "RUNTIME_IMAGE_ENV",
    "TAU_MANAGED_BY_LABEL",
    "TAU_MANAGED_BY_VALUE",
    "RAY_JOB_TTL_SECONDS",
    "SUBMISSION_MODE",
]
