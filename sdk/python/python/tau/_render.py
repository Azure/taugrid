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
import shlex
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
#: Mirrors core/workloadmeta. The CLI stamps these so a rendered workload is
#: identifiable as Tau-managed, discoverable by list commands, and legible to the
#: run-status/profile consumers. A producer that stamps only managed-by appears
#: in `tau run list` with an empty run ID and no profile provenance.
TAU_MANAGED_BY_LABEL = "tau.azure.com/managed-by"
TAU_MANAGED_BY_VALUE = "tau"
TAU_RUN_ID_LABEL = "tau.azure.com/run-id"
TAU_WORKLOAD_KIND_LABEL = "tau.azure.com/workload-kind"
TAU_PROFILE_LABEL = "tau.azure.com/profile"
TAU_TEAM_LABEL = "tau.azure.com/team"
TAU_LANE_LABEL = "tau.azure.com/lane"
TAU_WORKLOAD_PROFILE_ANNOTATION = "tau.azure.com/workload-profile"
TAU_WORKLOAD_PROFILE_SET_HASH_ANNOTATION = "tau.azure.com/workload-profile-set-hash"
TAU_CLUSTER_GENERATION_ANNOTATION = "tau.azure.com/tau-cluster-generation"
#: Mirrors experiment.WorkloadKindRayJob.
WORKLOAD_KIND_RAYJOB = "rayjob"
DEFAULT_IMAGE = "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0"
#: The certified runtime image is an operator decision, so it is overridable.
#: It must ship ray plus the notebook executor (nbformat/nbconvert/ipykernel).
RUNTIME_IMAGE_ENV = "TAUGRID_RUNTIME_IMAGE"
#: Operator-configured allow-list of Secret *names* a rendered pod may mount.
#: A caller-chosen name is refused because Kubernetes resolves a ``secretKeyRef``
#: with the node/kubelet identity, not the pod's ServiceAccount RBAC, so the
#: namespace a submission targets does not bound which Secret the notebook can
#: read. Unset or empty fails closed: no Secret refs are permitted.
ALLOWED_SECRETS_ENV = "TAUGRID_ALLOWED_SECRETS"


def default_image() -> str:
    """The runtime image, overridable by the operator via TAUGRID_RUNTIME_IMAGE."""
    import os

    return os.environ.get(RUNTIME_IMAGE_ENV, "").strip() or DEFAULT_IMAGE


def allowed_secret_names() -> frozenset:
    """Operator-configured Secret names a rendered pod may reference.

    Read from ``TAUGRID_ALLOWED_SECRETS`` at call time, so an operator change (or
    a test) takes effect without re-importing. An unset or empty value is the
    empty set, which fails closed: no ``secretKeyRef`` may be rendered.
    """
    import os

    raw = os.environ.get(ALLOWED_SECRETS_ENV, "")
    return frozenset(name.strip() for name in raw.split(",") if name.strip())


def _secret_refusal(key: str, secret_name: str, allowed: frozenset) -> str:
    """The refusal for a Secret name the operator did not allow.

    It names the operator action rather than dropping the env entry, because a
    silently dropped Secret leaves a run that fails confusingly inside the pod.
    """
    reason = (
        "Kubernetes resolves a secretKeyRef with the node/kubelet identity, not the "
        "pod's ServiceAccount RBAC, so the namespace this submission targets does not "
        "bound which Secret the notebook can read"
    )
    if not allowed:
        remedy = (
            f"{ALLOWED_SECRETS_ENV} is unset or empty, so this server currently permits no "
            f"Secret references. An operator must set {ALLOWED_SECRETS_ENV} to a "
            "comma-separated list of the Secret names a notebook may mount, and add "
            f"{secret_name!r} to it if that Secret is intended for notebook use"
        )
    else:
        listed = ",".join(sorted(allowed))
        remedy = (
            "only the operator-configured Secrets may be mounted. An operator must add "
            f"{secret_name!r} to {ALLOWED_SECRETS_ENV} (currently: {listed}) if that Secret "
            "is intended for notebook use"
        )
    return f"env_secret[{key}] references Secret {secret_name!r}, which is not permitted: {remedy}. {reason}."

#: Mirrors the CLI constants exactly.
RAY_JOB_TTL_SECONDS = 15
SUBMISSION_MODE = "HTTPMode"
#: How GPU work reaches the hardware for a notebook submission.
#:
#: The RayJob head is control-only by contract (num-cpus/num-gpus "0"), and the
#: notebook executes in that head through the staged runner. GPU sizing on a
#: profile therefore applies to Ray *workers*, never to the notebook's own
#: kernel. "ray" asserts the notebook dispatches its GPU work through Ray;
#: "local" is the default and is refused for a GPU profile, because the
#: alternative is a silent CPU fallback or a confusing CUDA failure.
GPU_EXECUTION_LOCAL = "local"
GPU_EXECUTION_RAY = "ray"
GPU_EXECUTION_MODES = (GPU_EXECUTION_LOCAL, GPU_EXECUTION_RAY)

#: Kueue metadata emitted by the CLI topology renderer. The notebook renderer
#: mirrors these values so a resolved profile's placement and workload priority
#: survive into the submitted RayJob instead of being silently flattened.
KUEUE_WORKLOAD_PRIORITY_LABEL = "kueue.x-k8s.io/priority-class"
KUEUE_REQUIRED_TOPOLOGY_ANNOTATION = "kueue.x-k8s.io/podset-required-topology"
KUEUE_UNCONSTRAINED_TOPOLOGY_ANNOTATION = "kueue.x-k8s.io/podset-unconstrained-topology"
_PLACEMENT_TOPOLOGIES = {
    "same-host": "kubernetes.io/hostname",
    "same-accelerator-domain": "tau.azure.com/accelerator-domain",
    "same-network-domain": "tau.azure.com/network-domain",
    "same-site": "tau.azure.com/site",
}
DURABLE_ROOT = "/data"
HOT_ROOT = "/mnt"
DSHM_MOUNT = "/dev/shm"
DSHM_SIZE = "16Gi"

HEAD_REQUESTS = {"cpu": "2", "memory": "8Gi"}
HEAD_LIMITS = {"cpu": "4", "memory": "16Gi"}

GPU_TOLERATIONS = (
    {"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
    {"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
)

_SECRET_REF_RE = re.compile(r"^[^\s:]+:[^\s:]+$")
_RAY_VERSION_RE = re.compile(r"-ray((\d+)\.(\d+)\.\d+)")
_SUPPORTED_RUNTIME_KEYS = {"pip"}
#: Mirrors the CLI's projectArchiveFilename. Ray resolves a file:// working_dir on
#: every node separately, so this is the one path the head and its workers agree on.
PROJECT_ARCHIVE_NAME = "_tau_project.zip"
PROJECT_ARCHIVE_PATH = f"{TARGET_DIR}/{PROJECT_ARCHIVE_NAME}"


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
    workload_priority_class: Optional[str] = None
    node_selector: Optional[Dict[str, str]] = None
    image: Optional[str] = None
    memory_per_worker: Optional[str] = None
    placement: Optional[str] = None

    def __post_init__(self):
        if type(self.workers) is not int or self.workers < 1 or type(self.gpus_per_worker) is not int or self.gpus_per_worker < 0:
            raise UnsupportedShape("workerCount must be a positive integer and gpusPerWorker a nonnegative integer")
        cpu = self.num_cpus_per_worker
        if cpu is not None and (type(cpu) not in (int, float) or not math.isfinite(cpu) or cpu <= 0):
            raise UnsupportedShape("cpusPerWorker must be a positive finite number")
        if self.node_selector is not None and (not isinstance(self.node_selector, dict) or not all(isinstance(key, str) and isinstance(value, str) for key, value in self.node_selector.items())):
            raise UnsupportedShape("nodeSelector must map strings to strings")
        if self.placement is not None and self.placement not in ("", "unconstrained", *_PLACEMENT_TOPOLOGIES):
            raise UnsupportedShape(
                f"placement {self.placement!r} is not representable by the notebook renderer; "
                "use unconstrained, same-host, same-accelerator-domain, same-network-domain, or same-site"
            )


def _workload_priority_labels(profile: Profile) -> Dict[str, str]:
    if not profile.workload_priority_class:
        return {}
    return {
        KUEUE_WORKLOAD_PRIORITY_LABEL: _require_label_value(
            "workload priority class", profile.workload_priority_class
        )
    }


def _topology_annotations(placement: Optional[str]) -> Dict[str, str]:
    if not placement:
        return {}
    if placement == "unconstrained":
        return {KUEUE_UNCONSTRAINED_TOPOLOGY_ANNOTATION: "true"}
    return {KUEUE_REQUIRED_TOPOLOGY_ANNOTATION: _PLACEMENT_TOPOLOGIES[placement]}


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
    gpu_execution: str = GPU_EXECUTION_LOCAL,
    run_id: Optional[str] = None,
    workload_kind: str = WORKLOAD_KIND_RAYJOB,
    team: Optional[str] = None,
    lane: Optional[str] = None,
    tau_cluster_generation: Optional[int] = None,
    profile_set_hash: Optional[str] = None,
) -> Dict[str, Any]:
    """Render a Kueue-admitted ray.io/v1 RayJob executing entrypoint.

    entrypoint is the base command that runs the notebook (typically
    python3 /script/_tau_runner.py --notebook analysis.ipynb). When payload is
    set it is embedded and the initContainer materialises it at /script; pip
    declarations add the CLI install preamble and runtimeEnvYAML.

    gpu_execution states where the notebook's GPU work actually runs. It must
    be "ray" for a profile with gpusPerWorker > 0, because the notebook runs in
    the control-only head and never receives a GPU itself.

    The shared workload metadata contract (core/workloadmeta) is stamped here
    rather than left to the caller: run identity, workload kind, the resolved
    profile, and the resolved profile-set provenance. The run list and the
    status/profile consumers read those keys, so a workload that omits them shows
    an empty run ID and an undeclared profile.
    """
    if gpu_execution not in GPU_EXECUTION_MODES:
        raise UnsupportedShape(
            f"gpu_execution must be one of {', '.join(GPU_EXECUTION_MODES)}; got {gpu_execution!r}"
        )
    if profile.gpus_per_worker > 0 and gpu_execution == GPU_EXECUTION_LOCAL:
        raise UnsupportedShape(
            f"profile {profile.name!r} requests {profile.gpus_per_worker} GPU(s) per worker, but a "
            "notebook always runs in this RayJob's control-only head, which is pinned to zero GPUs. "
            "The notebook's own kernel would get no GPU and would fall back to CPU or fail. "
            "Pass gpu_execution='ray' if the notebook dispatches its GPU work through Ray "
            "(ray.train / @ray.remote), or choose a profile with gpusPerWorker=0."
        )
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
    if profile.gpus_per_worker:
        worker_pod["tolerations"] = [dict(toleration) for toleration in GPU_TOLERATIONS]

    worker_distribution = payload if gpu_execution == GPU_EXECUTION_RAY else None
    if payload is not None:
        head_pod["initContainers"] = [_payload_init_container(image, payload)]
        head_pod["volumes"] = [_script_volume()]
        head_pod["containers"][0].setdefault("volumeMounts", []).append(
            {"name": VOLUME_NAME, "mountPath": TARGET_DIR, "readOnly": True}
        )
    if worker_distribution is not None:
        worker_pod["initContainers"] = [_payload_init_container(image, worker_distribution)]
        worker_pod["volumes"] = [_script_volume()]
        worker_pod["containers"][0].setdefault("volumeMounts", []).append(
            {"name": VOLUME_NAME, "mountPath": TARGET_DIR, "readOnly": True}
        )

    for pod in (head_pod, worker_pod):
        pod.setdefault("volumes", []).extend(_common_volumes())
        pod["containers"][0].setdefault("volumeMounts", []).extend(_common_mounts())
        if profile.node_selector:
            pod["nodeSelector"] = dict(profile.node_selector)
        if profile.priority_class:
            pod["priorityClassName"] = profile.priority_class

    topology_annotations = _topology_annotations(profile.placement)
    labels = _metadata_labels(name, queue, profile, run_id, workload_kind, team, lane)
    labels.update(_workload_priority_labels(profile))
    metadata: Dict[str, Any] = {
        "name": name,
        "namespace": namespace,
        "labels": labels,
    }
    annotations = _provenance_annotations(
        profile, tau_cluster_generation, profile_set_hash
    )
    annotations.update(topology_annotations)
    if payload is not None:
        annotations[ANNOTATION_DIGEST] = payload.digest
    if annotations:
        metadata["annotations"] = annotations

    pod_labels = {
        key: value for key, value in labels.items() if key == KUEUE_WORKLOAD_PRIORITY_LABEL
    }
    head_metadata: Dict[str, Any] = {}
    worker_metadata: Dict[str, Any] = {}
    if pod_labels:
        head_metadata["labels"] = dict(pod_labels)
        worker_metadata["labels"] = dict(pod_labels)
    if topology_annotations:
        worker_metadata["annotations"] = dict(topology_annotations)

    spec: Dict[str, Any] = {
        "suspend": True,
        "shutdownAfterJobFinishes": True,
        "ttlSecondsAfterFinished": int(ttl_seconds),
        "submissionMode": submission_mode,
        # KubeRay 1.6 moved the submitter Job's retry count to
        # submitterConfig.backoffLimit. Leaving it unset lets KubeRay's own
        # default (2) retry a deterministic notebook failure, and a later attempt
        # that re-attaches to the finished Ray job reports success, so a failed
        # notebook surfaced as SUCCEEDED.
        "submitterConfig": {"backoffLimit": 0},
        "entrypoint": _entrypoint(entrypoint, pip, submitter_shell=submission_mode == "K8sJobMode"),
        "rayClusterSpec": {
            "rayVersion": _ray_version(image),
            "headGroupSpec": {
                "rayStartParams": {"num-gpus": "0", "num-cpus": "0"},
                # Kueue reads placement and priority from the pod template's
                # metadata. Spreading these at the template top level puts them
                # where nothing looks, which drops co-location and admission
                # ordering without any visible error.
                "template": {"metadata": head_metadata, "spec": head_pod},
            },
            "workerGroupSpecs": [
                {
                    "replicas": profile.workers,
                    "groupName": "notebook-workers",
                    "rayStartParams": {"num-gpus": num_gpus},
                    "template": {"metadata": worker_metadata, "spec": worker_pod},
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
    if pip or worker_distribution is not None:
        spec["runtimeEnvYAML"] = _runtime_env_yaml(
            pip, distributed=worker_distribution is not None
        )

    return {"apiVersion": "ray.io/v1", "kind": "RayJob", "metadata": metadata, "spec": spec}


def _metadata_labels(
    name: str,
    queue: str,
    profile: Profile,
    run_id: Optional[str],
    workload_kind: str,
    team: Optional[str],
    lane: Optional[str],
) -> Dict[str, str]:
    """The workload metadata labels every Tau renderer must agree on.

    run-id defaults to the object name, because that is the identity the CLI
    stamps and the run list reads. Without it a Tau-managed RayJob appears in
    ``tau run list`` with an empty run ID and cannot be deduplicated.
    """
    labels: Dict[str, str] = {
        TAU_QUEUE_LABEL: _require_label_value("queue", queue),
        TAU_MANAGED_BY_LABEL: TAU_MANAGED_BY_VALUE,
        TAU_RUN_ID_LABEL: _require_label_value("run_id", run_id or name),
        TAU_PROFILE_LABEL: _require_label_value("profile name", profile.name),
    }
    if workload_kind:
        labels[TAU_WORKLOAD_KIND_LABEL] = _require_label_value("workload_kind", workload_kind)
    if team:
        labels[TAU_TEAM_LABEL] = _require_label_value("team", team)
    if lane:
        labels[TAU_LANE_LABEL] = _require_label_value("lane", lane)
    return labels


#: Kubernetes label values: alphanumerics, '-', '_', '.' with alphanumeric edges,
#: at most 63 characters. Mirrors experiment.addLabel, which drops an invalid
#: value; a renderer must refuse instead, because the dropped value here is the
#: run identity a consumer uses and a silent drop is the defect being fixed.
_LABEL_VALUE_RE = re.compile(r"[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?")


def _require_label_value(field: str, value: str) -> str:
    """Refuse a label value that Kubernetes would reject, without altering it.

    Identity values are not normalized or truncated: narrowing run-id, profile,
    team, or lane would collide two different runs or grant the wrong capacity.
    An invalid value is a loud refusal at plan time instead of an opaque apply
    failure.
    """
    text = str(value or "")
    if not _LABEL_VALUE_RE.fullmatch(text):
        raise UnsupportedShape(
            f"{field} {text!r} is not a valid Kubernetes label value; it must be 1-63 "
            "characters of alphanumerics, '-', '_' or '.', starting and ending with an "
            "alphanumeric"
        )
    return text


def _provenance_annotations(
    profile: Profile,
    tau_cluster_generation: Optional[int],
    profile_set_hash: Optional[str],
) -> Dict[str, str]:
    """Resolved-profile provenance the status/profile consumers display."""
    annotations: Dict[str, str] = {TAU_WORKLOAD_PROFILE_ANNOTATION: profile.name}
    if isinstance(tau_cluster_generation, int) and not isinstance(tau_cluster_generation, bool):
        annotations[TAU_CLUSTER_GENERATION_ANNOTATION] = str(tau_cluster_generation)
    if profile_set_hash:
        annotations[TAU_WORKLOAD_PROFILE_SET_HASH_ANNOTATION] = str(profile_set_hash)
    return annotations


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
    # The Secret *name* is an operator decision, so it is confined to
    # TAUGRID_ALLOWED_SECRETS here, at the one place a SECRET:KEY becomes a
    # secretKeyRef. The key inside an allowed Secret stays caller-chosen: the
    # Secret is the privilege boundary, because referencing any key makes the
    # kubelet resolve the whole Secret object.
    allowed = allowed_secret_names()
    for key in sorted(secret):
        spec = str(secret[key])
        if not _SECRET_REF_RE.match(spec):
            raise UnsupportedShape(f"env_secret[{key}] must use the SECRET:KEY form, got {spec!r}")
        secret_name, _, secret_key = spec.partition(":")
        if secret_name not in allowed:
            raise UnsupportedShape(_secret_refusal(key, secret_name, allowed))
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


def _entrypoint(base: str, pip: List[str], *, submitter_shell: bool = False) -> str:
    """Prepend the CLI's pip preamble to the base notebook command.

    ``submitter_shell`` is set for K8sJobMode. KubeRay interpolates the
    entrypoint verbatim into the submitter's ``ray job submit ... -- <entrypoint>``
    shell command, so a raw multi-line script ends that command at the first
    newline: the dependency install and the notebook runner would then execute in
    the submitter pod (1 CPU/1Gi, unable to reach the intended Ray cluster) rather
    than on the Ray head. ``shlex.quote`` wraps the whole script in one shell word,
    so the newlines stay inside a single argument and the head still receives the
    identical script.
    """
    lines = ["set -eu"]
    lines.append(
        "if [ -s /script/requirements.txt ]; then "
        "python3 -m pip install --quiet --no-cache-dir -r /script/requirements.txt || "
        "python3 -m pip install --quiet --no-cache-dir --user -r /script/requirements.txt; fi"
    )
    lines.append('PATH="$(python3 -m site --user-base)/bin:$PATH"; export PATH')
    if pip:
        quoted = " ".join(shlex.quote(str(pkg)) for pkg in pip)
        lines.append(
            f"python3 -m pip install --quiet --no-cache-dir {quoted} || "
            f"python3 -m pip install --quiet --no-cache-dir --user {quoted}"
        )
    lines.append("cd " + DURABLE_ROOT)
    lines.append(base)
    script = "\n".join(lines) + "\n"
    if submitter_shell:
        return "bash -lc " + shlex.quote(script)
    return script


def _runtime_env_yaml(pip: List[str], *, distributed: bool = False) -> str:
    runtime: Dict[str, Any] = {"pip": list(pip)}
    if distributed:
        runtime["working_dir"] = f"file://{PROJECT_ARCHIVE_PATH}"
        runtime["env_vars"] = {ENV_TARGET_DIR: TARGET_DIR}
    return yaml.safe_dump(runtime, sort_keys=False)


def _ray_version(image: str) -> str:
    match = _RAY_VERSION_RE.search(image)
    return match.group(1) if match else "2.56.0"


__all__ = [
    "UnsupportedShape",
    "Profile",
    "render_rayjob",
    "refuse",
    "TAU_QUEUE_LABEL",
    "default_image",
    "RUNTIME_IMAGE_ENV",
    "ALLOWED_SECRETS_ENV",
    "allowed_secret_names",
    "TAU_MANAGED_BY_LABEL",
    "TAU_MANAGED_BY_VALUE",
    "TAU_RUN_ID_LABEL",
    "TAU_WORKLOAD_KIND_LABEL",
    "TAU_PROFILE_LABEL",
    "TAU_TEAM_LABEL",
    "TAU_LANE_LABEL",
    "TAU_WORKLOAD_PROFILE_ANNOTATION",
    "TAU_WORKLOAD_PROFILE_SET_HASH_ANNOTATION",
    "TAU_CLUSTER_GENERATION_ANNOTATION",
    "WORKLOAD_KIND_RAYJOB",
    "RAY_JOB_TTL_SECONDS",
    "SUBMISSION_MODE",
    "GPU_EXECUTION_LOCAL",
    "GPU_EXECUTION_RAY",
    "GPU_EXECUTION_MODES",
]
