# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Run status model + reader for the notebook widget.

The reader is a pure function of an injectable ClusterClient-shaped object,
so tests can drive it with a recorded fake offline (no cluster, no network).

Two capabilities live here:

* read_run_status normalizes one RayJob (plus its pods) into a RunStatus. It
  never raises: a missing object becomes state="not_submitted" and a read
  failure becomes a diagnostic.
* list_runs discovers RayJobs in a namespace so the panel can offer
  "load an existing run" without the caller knowing the name up front.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

from tau.widgets.kube import ClusterClient

RAY_GROUP = "ray.io"
RAY_VERSION = "v1"
RAY_PLURAL = "rayjobs"

#: Coarse states the panel renders. not_submitted means no object was found.
TERMINAL_STATES = ("complete", "failed")

#: RayJob status.jobStatus values mapped onto the coarse states.
_COMPLETE_STATUSES = ("SUCCEEDED", "COMPLETED", "SUCCESS")
_FAILED_STATUSES = ("FAILED", "DEAD", "ERROR")
_QUEUED_STATUSES = ("PENDING", "SUSPENDED", "QUEUED")


@dataclass
class Diagnostic:
    """A user-facing observation about the run, with a suggested next step."""

    code: str
    severity: str  # info | warn | error
    message: str
    suggestion: Optional[str] = None


@dataclass
class GPUDevice:
    """Per-device GPU observation (mirrors metrics.gpuRuntime.devices)."""

    pod: Optional[str] = None
    gpu: Optional[str] = None
    utilization_percent: Optional[float] = None
    utilization_observed: bool = False
    framebuffer_used_mib: Optional[float] = None
    framebuffer_used_observed: bool = False


@dataclass
class PodInfo:
    name: str = ""
    phase: Optional[str] = None
    node: Optional[str] = None
    ready: bool = False
    restarts: int = 0
    ray_node_type: Optional[str] = None


@dataclass
class RunStatus:
    """Normalized status for one RayJob, consumed by the panel."""

    existing: bool = False
    name: str = ""
    namespace: str = ""
    state: Optional[str] = None  # queued | running | failed | complete | not_submitted
    display_state: Optional[str] = None
    ray_cluster_name: Optional[str] = None
    job_id: Optional[str] = None
    deployment_status: Optional[str] = None
    queue: Optional[str] = None
    admitted: Optional[bool] = None
    message: Optional[str] = None
    reason: Optional[str] = None
    pods: List[PodInfo] = field(default_factory=list)
    gpu_devices: List[GPUDevice] = field(default_factory=list)
    diagnostics: List[Diagnostic] = field(default_factory=list)

    @property
    def terminal(self) -> bool:
        """True once the run reached an authoritative terminal state."""
        return self.state in TERMINAL_STATES

    @property
    def ready_pods(self) -> int:
        return sum(1 for pod in self.pods if pod.ready)

    @property
    def total_pods(self) -> int:
        return len(self.pods)


@dataclass(frozen=True)
class RunSummary:
    """A lightweight row for the run chooser (list_runs)."""

    name: str
    namespace: str
    state: Optional[str] = None
    queue: Optional[str] = None
    created: Optional[str] = None


def classify(rayjob: Optional[Dict[str, Any]]) -> str:
    """Map a RayJob document to one of the widget's coarse states."""
    if not rayjob:
        return "not_submitted"
    status = rayjob.get("status") or {}
    job = str(status.get("jobStatus") or "").upper()
    deployment = str(status.get("jobDeploymentStatus") or "").upper()
    reason = str(status.get("reason") or "").upper()
    if job in (*_FAILED_STATUSES, "STOPPED", "CANCELLED", "CANCELED") or deployment == "FAILED" or reason in ("SUBMISSIONFAILED", "DEADLINEEXCEEDED", "BACKOFFLIMITEXCEEDED"):
        return "failed"
    if job in _COMPLETE_STATUSES or deployment in ("COMPLETE", "COMPLETED"):
        return "complete"
    if job == "RUNNING" or (not job and deployment == "RUNNING"):
        return "running"
    if (not job and not deployment) or job in _QUEUED_STATUSES or deployment in ("INITIALIZING", "SUSPENDED", "WAITFORCLUSTER", "WAITFORUSER") or (rayjob.get("spec") or {}).get("suspend"):
        return "queued"
    return "unknown"


def read_run_status(
    client: ClusterClient,
    *,
    namespace: str,
    name: str,
) -> RunStatus:
    """Build a normalized RunStatus from the cluster client.

    The fake client used in offline tests records
    get_namespaced_custom_object and list_namespaced_pod returns; a missing
    object (None or an ApiException with status 404) yields existing=False.
    Never raises: any other read failure is recorded as a diagnostic so the
    panel can show a partial status.
    """
    info = RunStatus(name=name, namespace=namespace)

    rayjob: Optional[Dict[str, Any]] = None
    try:
        rayjob = client.custom.get_namespaced_custom_object(
            group=RAY_GROUP, version=RAY_VERSION, namespace=namespace, plural=RAY_PLURAL, name=name
        )
        info.existing = bool(rayjob)
        info.state = classify(rayjob)
    except Exception as exc:  # missing object or a read failure
        if _is_not_found(exc):
            info.existing = False
            info.state = "not_submitted"
        else:
            info.existing = False
            info.state = "unknown"
            info.diagnostics.append(
                Diagnostic(
                    code="status-read-failed",
                    severity="error",
                    message=f"could not read RayJob {namespace}/{name}: {exc}",
                    suggestion="check cluster connectivity and RBAC (get rayjobs.ray.io)",
                )
            )

    if rayjob:
        _fill_from_rayjob(info, rayjob)
        _read_pods(client, info)
        _derive_diagnostics(info)

    info.display_state = (info.state or "unknown").lower()
    return info


def list_runs(client: ClusterClient, *, namespace: str, label_selector: Optional[str] = None) -> List[RunSummary]:
    """List RayJobs in namespace as lightweight summaries.

    Returns an empty list (never raises) when the listing fails, so a run
    chooser can degrade to "enter a name manually".
    """
    kwargs: Dict[str, Any] = {"group": RAY_GROUP, "version": RAY_VERSION, "namespace": namespace, "plural": RAY_PLURAL}
    if label_selector:
        kwargs["label_selector"] = label_selector
    try:
        listing = client.custom.list_namespaced_custom_object(**kwargs)
    except Exception:
        return []

    rows: List[RunSummary] = []
    for item in _items(listing):
        if not isinstance(item, dict):
            continue
        metadata = item.get("metadata") or {}
        row_name = str(metadata.get("name") or "")
        if not row_name:
            continue
        labels = metadata.get("labels") or {}
        rows.append(
            RunSummary(
                name=row_name,
                namespace=namespace,
                state=classify(item),
                queue=labels.get("kueue.x-k8s.io/queue-name"),
                created=metadata.get("creationTimestamp"),
            )
        )
    return rows


def _fill_from_rayjob(info: RunStatus, rayjob: Dict[str, Any]) -> None:
    metadata = rayjob.get("metadata") or {}
    status = rayjob.get("status") or {}
    labels = metadata.get("labels") or {}

    info.queue = labels.get("kueue.x-k8s.io/queue-name") or info.queue
    info.ray_cluster_name = _opt_str(status.get("rayClusterName"))
    info.job_id = _opt_str(status.get("jobId"))
    info.deployment_status = _opt_str(status.get("jobDeploymentStatus"))
    info.reason = _opt_str(status.get("reason"))
    info.message = _opt_str(status.get("message"))

    # Kueue admission: a suspended RayJob that has been admitted loses the
    # suspend flag; the condition is the authoritative signal when present.
    conditions = status.get("conditions") or []
    for condition in conditions:
        if not isinstance(condition, dict):
            continue
        if str(condition.get("type", "")).lower() == "admitted":
            info.admitted = str(condition.get("status", "")).lower() == "true"
            if condition.get("message"):
                info.message = _opt_str(condition.get("message"))
            break


def _read_pods(client: ClusterClient, info: RunStatus) -> None:
    """Read pods for the run, trying each plausible label selector once."""
    seen: Dict[str, PodInfo] = {}
    for selector in _pod_selectors(info.name, info.ray_cluster_name):
        try:
            listing = client.core.list_namespaced_pod(info.namespace, label_selector=selector)
        except Exception:
            continue
        for pod in _items(listing):
            pod_info = _pod_info(pod)
            if pod_info.name and pod_info.name not in seen:
                seen[pod_info.name] = pod_info
    info.pods = sorted(seen.values(), key=lambda p: p.name)


def _pod_selectors(name: str, ray_cluster_name: Optional[str]) -> List[str]:
    selectors = [f"job-name={name}"]
    if ray_cluster_name:
        selectors.insert(0, f"ray.io/cluster={ray_cluster_name}")
    # De-dup while preserving order (the cluster selector is the most precise).
    ordered: List[str] = []
    for selector in selectors:
        if selector not in ordered:
            ordered.append(selector)
    return ordered


def _pod_info(pod: Any) -> PodInfo:
    """Normalize a pod from either a dict (fakes) or a V1Pod (real SDK)."""
    metadata = _field(pod, "metadata") or {}
    spec = _field(pod, "spec") or {}
    status = _field(pod, "status") or {}
    labels = _field(metadata, "labels") or {}

    container_statuses = _field(status, "container_statuses", "containerStatuses") or []
    restarts = sum(int(_field(c, "restart_count", "restartCount") or 0) for c in container_statuses)
    ready = _pod_ready(status, container_statuses)
    return PodInfo(
        name=str(_field(metadata, "name") or ""),
        phase=_opt_str(_field(status, "phase")),
        node=_opt_str(_field(spec, "node_name", "nodeName")),
        ready=ready,
        restarts=restarts,
        ray_node_type=_opt_str(labels.get("ray.io/node-type") if isinstance(labels, dict) else None),
    )


def _pod_ready(status: Any, container_statuses: List[Any]) -> bool:
    for condition in _field(status, "conditions") or []:
        if str(_field(condition, "type") or "").lower() == "ready":
            return str(_field(condition, "status") or "").lower() == "true"
    if container_statuses:
        return all(bool(_field(c, "ready")) for c in container_statuses)
    return False


def _field(obj: Any, snake: str, camel: Optional[str] = None) -> Any:
    """Read a field from a dict (camelCase key) or an SDK object (snake_case)."""
    if obj is None:
        return None
    if isinstance(obj, dict):
        if camel is not None and camel in obj:
            return obj[camel]
        return obj.get(snake)
    return getattr(obj, snake, None)


def _derive_diagnostics(info: RunStatus) -> None:
    if info.state == "failed":
        info.diagnostics.append(
            Diagnostic(
                code="rayjob-failed",
                severity="error",
                message=info.message or info.reason or "the RayJob reported a failed status",
                suggestion="open the pod logs for the failing container and fix the entrypoint",
            )
        )
    elif info.state == "queued":
        info.diagnostics.append(
            Diagnostic(
                code="awaiting-admission",
                severity="info",
                message=info.message or "waiting for Kueue to admit the workload",
                suggestion=f"check queue {info.queue or 'default'} quota and pending workloads",
            )
        )
    elif info.state == "complete":
        info.diagnostics.append(
            Diagnostic(code="rayjob-complete", severity="info", message="the RayJob completed successfully")
        )

    restarted = [pod.name for pod in info.pods if pod.restarts > 0]
    if restarted:
        info.diagnostics.append(
            Diagnostic(
                code="pod-restarts",
                severity="warn",
                message=f"{len(restarted)} pod(s) restarted: {', '.join(restarted)}",
                suggestion="inspect the previous container logs for the cause",
            )
        )


def _is_not_found(exc: Exception) -> bool:
    """True for a Kubernetes 404 (missing object), without importing the SDK."""
    status = getattr(exc, "status", None)
    if status == 404:
        return True
    reason = getattr(exc, "reason", None)
    if reason and "not found" in str(reason).lower():
        return True
    return "not found" in str(exc).lower()


def _items(listing: Any) -> list:
    if isinstance(listing, dict):
        return listing.get("items") or []
    items = getattr(listing, "items", None)
    return list(items) if items is not None else []


def _opt_str(value: Any) -> Optional[str]:
    return None if value in (None, "") else str(value)


__all__ = [
    "RunStatus",
    "RunSummary",
    "GPUDevice",
    "PodInfo",
    "Diagnostic",
    "read_run_status",
    "list_runs",
    "classify",
    "TERMINAL_STATES",
    "ClusterClient",
]
