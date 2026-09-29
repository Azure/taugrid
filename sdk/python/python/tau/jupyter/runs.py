# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded, read-only workload evidence for the native notebook console."""

from __future__ import annotations

import os
import re
import time
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import urlsplit

from tau._kube_io import bounded_body, read_document, timeout
from tau.widgets.status import Diagnostic, RunStatus, _field, _fill_from_rayjob, _pod_info, classify

LIMIT = 500
LOG_BYTES = 65536
QUEUE = "kueue.x-k8s.io/queue-name"


class ReadError(Exception):
    def __init__(self, message: str, status: int = 400):
        super().__init__(message)
        self.status = status


@dataclass
class NativeStatus(RunStatus):
    kind: str = "RayJob"
    phases: list[dict] = field(default_factory=list)
    output: dict = field(default_factory=dict)
    containers: dict[str, list[str]] = field(default_factory=dict)
    uid: str | None = None
    pod_uids: dict[str, str] = field(default_factory=dict)
    pod_roles: dict[str, str] = field(default_factory=dict)
    metric_sources: list[dict] = field(default_factory=list)
    metric_discovery_error: bool = False
    annotations: dict = field(default_factory=dict)
    log_sources: dict = field(default_factory=dict)

    @property
    def ready_pods(self) -> int:
        return sum(pod.ready for pod in self.pods if self.pod_roles.get(pod.name) != "submitter")

    @property
    def total_pods(self) -> int:
        return sum(self.pod_roles.get(pod.name) != "submitter" for pod in self.pods)


def portal_url() -> str | None:
    value = os.environ.get("TAUGRID_PORTAL_URL", "").strip()
    try:
        parsed = urlsplit(value)
        if parsed.scheme in ("http", "https") and parsed.hostname and not parsed.username and not parsed.password and not re.search(r"[\s\\]", value):
            return value
    except ValueError:
        pass
    return None


def validate_target(namespace: str, name: str | None = None, kind: str = "RayJob") -> None:
    if kind not in ("Job", "RayJob"):
        raise ReadError("kind must be Job or RayJob")
    for label, value, maximum, pattern in (
        ("namespace", namespace, 63, r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?"),
        ("name", name, 253, r"[a-z0-9](?:[-.a-z0-9]*[a-z0-9])?"),
    ):
        if value is not None and (len(value) > maximum or not re.fullmatch(pattern, value)):
            raise ReadError(f"invalid {label}")


NAMESPACE_LABEL = "tau.azure.com/workspace"


def list_namespaces(client: Any) -> dict:
    """Readable namespaces, flagged when they are set up to host Tau runs.

    A namespace can host Tau work when it carries the label the ClusterQueue
    selects on, so the picker can put usable destinations first. The label is a
    hint, not an authorization boundary: the Kubernetes API still decides what
    the caller may read.
    """
    try:
        listing = read_document(client.core.list_namespace, limit=LIMIT)
    except Exception as exc:
        return {"namespaces": [], "warnings": [f"namespace discovery failed: {exc}"]}
    rows: list[dict] = []
    for item in listing_items(listing)[:LIMIT]:
        name = _field(metadata(item), "name")
        if not name:
            continue
        labels = _field(metadata(item), "labels") or {}
        rows.append({"name": str(name), "tauEnabled": NAMESPACE_LABEL in labels})
    rows.sort(key=lambda row: (not row["tauEnabled"], row["name"]))
    warnings = []
    if truncated(listing) or len(listing_items(listing)) >= LIMIT:
        warnings.append(f"Namespace discovery reached the {LIMIT}-namespace limit.")
    return {"namespaces": rows, "warnings": warnings}


def metadata(obj: Any) -> Any:
    return _field(obj, "metadata") or {}


def owned(obj: Any, kind: str, uid: str | None) -> bool:
    return bool(uid) and any(
        _field(ref, "kind") == kind and _field(ref, "uid") == uid and _field(ref, "controller") is True
        for ref in (_field(metadata(obj), "owner_references", "ownerReferences") or [])
    )


def custom_get(client: Any, namespace: str, name: str, plural: str) -> Any:
    return read_document(client.custom.get_namespaced_custom_object, group="ray.io", version="v1", namespace=namespace, plural=plural, name=name)


def listing_items(listing: Any) -> list:
    return _field(listing, "items") or []


def truncated(listing: Any) -> bool:
    return bool(_field(metadata(listing), "_continue", "continue"))


def job_state(obj: Any) -> str:
    status = _field(obj, "status") or {}
    conditions = _field(status, "conditions") or []
    for condition_type, state in (("Failed", "failed"), ("Complete", "complete"), ("Suspended", "queued")):
        if any(_field(condition, "type") == condition_type and str(_field(condition, "status")).lower() == "true" for condition in conditions):
            return state
    return "running" if (_field(status, "active") or 0) > 0 else "queued"


def ray_state(obj: dict) -> str:
    return classify(obj)

def list_runs(client: Any, *, namespace: str, queue: str = "") -> dict:
    validate_target(namespace)
    rows, warnings = [], []
    limited = False
    for kind in ("Job", "RayJob"):
        try:
            if kind == "Job":
                listing = read_document(client.batch.list_namespaced_job, namespace=namespace, limit=LIMIT)
            else:
                listing = read_document(client.custom.list_namespaced_custom_object, group="ray.io", version="v1", plural="rayjobs", namespace=namespace, limit=LIMIT)
            items = listing_items(listing)
            limited |= truncated(listing) or len(items) > LIMIT
            for obj in items[:LIMIT]:
                meta = metadata(obj)
                labels = _field(meta, "labels") or {}
                refs = _field(meta, "owner_references", "ownerReferences") or []
                if not any(key.startswith("tau.azure.com/") for key in labels):
                    continue
                if kind == "Job" and any(_field(ref, "kind") == "RayJob" for ref in refs):
                    continue
                if queue and labels.get(QUEUE) != queue:
                    continue
                state = job_state(obj) if kind == "Job" else ray_state(obj)
                created = _field(meta, "creation_timestamp", "creationTimestamp")
                rows.append({"name": _field(meta, "name"), "namespace": namespace, "kind": kind, "state": state, "queue": labels.get(QUEUE), "created": created.isoformat() if hasattr(created, "isoformat") else created})
        except Exception:
            warnings.append(f"Could not list {kind} resources. Check API availability and namespace read permissions.")
    rows.sort(key=lambda row: (row["created"] or "", row["name"] or "", row["kind"]), reverse=True)
    return {"runs": rows, "warnings": warnings, "truncated": limited}


def observe(info: NativeStatus, key: str, label: str, state: str, detail: str, hint: str = "") -> None:
    info.phases.append({"key": key, "label": label, "state": state, "detail": detail, "hint": hint})


def warning(info: NativeStatus, code: str, detail: str) -> None:
    info.diagnostics.append(Diagnostic(code=code, severity="warn", message=detail, suggestion="Check API availability and namespace read permissions, then refresh."))


def read_admission(client: Any, info: NativeStatus, uid: str | None) -> None:
    if not info.queue:
        observe(info, "admission", "Admission", "skipped", "No LocalQueue is recorded.")
        return
    detail = "No matching Workload admission evidence was returned."
    try:
        listing = read_document(client.custom.list_namespaced_custom_object, group="kueue.x-k8s.io", version="v1beta1", plural="workloads", namespace=info.namespace, limit=LIMIT)
        if truncated(listing):
            warning(info, "workloads-truncated", "Workload discovery is partial.")
        matches = [obj for obj in listing_items(listing)[:LIMIT] if owned(obj, info.kind, uid)]
        if len(matches) == 1:
            for condition in matches[0].get("status", {}).get("conditions", []):
                if condition.get("type") == "Admitted":
                    info.admitted = {"True": True, "False": False}.get(condition.get("status"))
                    detail = condition.get("message") or condition.get("reason") or "Kueue reported admission."
        elif len(matches) > 1:
            info.admitted = None
            detail = "Multiple Workloads match this owner; admission is ambiguous."
    except Exception:
        warning(info, "admission-unavailable", "Kueue Workload evidence could not be read.")
    state = "done" if info.admitted is True else "pending" if info.admitted is False else "unknown"
    if info.admitted is not None and detail.startswith("No matching"):
        detail = "RayJob Admitted condition (no matching Workload evidence)."
    observe(info, "admission", "Admission", state, detail, "Check LocalQueue quota and admission checks." if state != "done" else "")


def discover_submitters(client: Any, info: NativeStatus, obj: Any) -> list:
    if info.kind != "RayJob":
        return []
    result = []
    try:
        listing = read_document(client.batch.list_namespaced_job, namespace=info.namespace, limit=LIMIT)
        if truncated(listing) or len(listing_items(listing)) >= LIMIT:
            raise ReadError("Submitter Job discovery was truncated.")
        jobs = [job for job in listing_items(listing) if owned(job, "RayJob", info.uid)]
        if len(jobs) > 1:
            raise ReadError("Multiple submitter Jobs; source is ambiguous.")
        for job in jobs:
            job_meta = metadata(job)
            pods = read_document(client.core.list_namespaced_pod, namespace=info.namespace, label_selector=f"job-name={_field(job_meta, 'name')}", limit=LIMIT)
            if truncated(pods) or len(listing_items(pods)) >= LIMIT:
                raise ReadError("Submitter pod discovery was truncated.")
            for pod in listing_items(pods):
                if owned(pod, "Job", _field(job_meta, "uid")):
                    result.append(pod)
                    record_metric_source(info, pod, "submitter", "Job", _field(job_meta, "uid"))
    except Exception:
        info.metric_discovery_error = True
        warning(info, "submitter-unavailable", "Submitter ownership could not be fully verified; loss evidence is unavailable.")
    return result


def record_metric_source(info: NativeStatus, pod: Any, role: str, owner_kind: str, owner_uid: str) -> None:
    meta = metadata(pod)
    name, uid = _field(meta, "name"), _field(meta, "uid")
    record_log_source(info, pod)
    info.pod_roles[name] = role
    info.pod_uids[name] = uid
    containers = _field(_field(pod, "spec"), "containers") or []
    if not uid or len(containers) != 1 or not _field(containers[0], "name"):
        info.metric_discovery_error = True
    if uid and len(containers) == 1:
        info.metric_sources.append({"type": "stdout", "runUid": info.uid, "pod": name,
                                    "podUid": uid, "container": _field(containers[0], "name"),
                                    "ownerKind": owner_kind, "ownerUid": owner_uid})


def discover_pods(client: Any, info: NativeStatus, obj: Any) -> list:
    uid = _field(metadata(obj), "uid")
    owner_kind = info.kind
    if info.kind == "Job":
        selector = f"job-name={info.name}"
        observe(info, "cluster", "Ray cluster", "skipped", "Batch Job does not need a RayCluster.")
    else:
        if not info.ray_cluster_name:
            observe(info, "cluster", "Ray cluster", "skipped" if info.terminal else "pending", "No RayCluster name is currently recorded.", "Check RayJob reconciliation." if not info.terminal else "")
            return []
        try:
            cluster = custom_get(client, info.namespace, info.ray_cluster_name, "rayclusters")
            if not owned(cluster, "RayJob", uid):
                raise ReadError("RayCluster ownership does not match this run.")
            uid = _field(metadata(cluster), "uid")
            owner_kind = "RayCluster"
            selector = f"ray.io/cluster={info.ray_cluster_name}"
            observe(info, "cluster", "Ray cluster", "done", "Owned RayCluster exists; creation does not imply readiness.")
        except Exception as exc:
            state = "skipped" if info.terminal and getattr(exc, "status", None) == 404 else "unknown"
            observe(info, "cluster", "Ray cluster", state, "RayCluster removed after execution." if state == "skipped" else "RayCluster existence or ownership could not be verified.")
            if state == "unknown":
                warning(info, "cluster-unavailable", "RayCluster existence or ownership could not be verified.")
            return []
    listing = read_document(client.core.list_namespaced_pod, namespace=info.namespace, label_selector=selector, limit=LIMIT)
    if truncated(listing) or len(listing_items(listing)) > LIMIT:
        if info.kind == "Job":
            info.metric_discovery_error = True
        warning(info, "pods-truncated", "Pod discovery is partial; counts describe only returned pods.")
    return [pod for pod in listing_items(listing)[:LIMIT] if owned(pod, owner_kind, uid)]


def pod_containers(pod: Any) -> list[str]:
    spec = _field(pod, "spec") or {}
    return [_field(container, "name") for key, camel in (("containers", "containers"), ("init_containers", "initContainers")) for container in (_field(spec, key, camel) or []) if _field(container, "name")]


def read_run(client: Any, *, namespace: str, name: str, kind: str = "RayJob") -> NativeStatus:
    validate_target(namespace, name, kind)
    info = NativeStatus(name=name, namespace=namespace, kind=kind)
    try:
        obj = read_document(client.batch.read_namespaced_job, namespace=namespace, name=name) if kind == "Job" else custom_get(client, namespace, name, "rayjobs")
    except Exception as exc:
        if getattr(exc, "status", None) == 404:
            info.state = info.display_state = "not_submitted"
            return info
        raise ReadError(f"Could not read {kind}. Check namespace, cluster connection and read permissions.", 502) from exc
    info.existing = True
    if kind == "RayJob":
        _fill_from_rayjob(info, obj)
        info.state = ray_state(obj)
    else:
        info.state = job_state(obj)
    info.display_state = info.state
    meta = metadata(obj)
    info.uid = _field(meta, "uid")
    info.queue = (_field(meta, "labels") or {}).get(QUEUE)
    annotations = _field(meta, "annotations") or {}
    info.annotations = annotations
    info.output = {"path": annotations.get("tau.azure.com/result-path"), "pvc": annotations.get("tau.azure.com/result-pvc"), "portalUrl": portal_url()}
    read_admission(client, info, _field(meta, "uid"))
    spec = _field(obj, "spec") or {}
    remote = _field(spec, "managed_by", "managedBy") == "kueue.x-k8s.io/multikueue"
    if remote:
        observe(info, "cluster", "Ray cluster", "unknown" if kind == "RayJob" else "skipped", "Manager view: worker evidence is remote.")
        observe(info, "pods", "Pods", "unknown", "Manager view: worker pods are remote, not locally observed.")
    else:
        try:
            pods = discover_pods(client, info, obj)
            if kind == "Job":
                for pod in pods:
                    record_metric_source(info, pod, "worker", "Job", info.uid)
            for pod in pods:
                record_log_source(info, pod)
            info.pods = [_pod_info(pod) for pod in pods]
            info.containers = {_field(metadata(pod), "name"): pod_containers(pod) for pod in pods}
            reasons = []
            for pod in pods:
                status = _field(pod, "status") or {}
                for condition in _field(status, "conditions") or []:
                    if _field(condition, "type") == "PodScheduled" and str(_field(condition, "status")) == "False":
                        reasons.append(_field(condition, "message") or "Pod is not scheduled")
                for key, camel in (("init_container_statuses", "initContainerStatuses"), ("container_statuses", "containerStatuses")):
                    for container in _field(status, key, camel) or []:
                        waiting = _field(_field(container, "state"), "waiting")
                        if waiting:
                            reasons.append(_field(waiting, "reason") or "Container waiting")
            state = "warning" if reasons else "done" if pods and info.ready_pods == info.total_pods else "active" if pods else "skipped" if info.terminal else "unknown"
            detail = "; ".join(reasons) if reasons else f"{info.ready_pods}/{info.total_pods} observed pods ready." if pods else "No owned pods returned; they may not exist yet or may have been removed."
            observe(info, "pods", "Pods", state, detail, "Check scheduling, image access and container startup." if state in ("warning", "active") else "")
        except Exception:
            info.metric_discovery_error = True
            warning(info, "pods-unavailable", "Pod evidence could not be read.")
        observe(info, "pods", "Pods", "unknown", "Pod evidence could not be read.")
    if kind == "RayJob":
        submitters = discover_submitters(client, info, obj)
        info.pods.extend(_pod_info(pod) for pod in submitters)
        info.containers.update({_field(metadata(pod), "name"): pod_containers(pod) for pod in submitters})
    execution_state = "done" if info.state == "complete" else "warning" if info.state == "failed" else "unknown" if remote else "active" if info.state == "running" else "pending"
    detail = "Execution is remote; manager status does not prove worker health." if remote and not info.terminal else f"{kind} reports {info.state}. Pod readiness alone does not prove execution success."
    observe(info, "execution", "Execution", execution_state, detail, "Inspect selected container logs." if info.state == "failed" else "")
    return info


def read_logs(client: Any, *, namespace: str, name: str, pod: str, container: str, kind: str = "RayJob", tail: int = 200, previous: bool = False, timestamps: bool = False) -> dict:
    if not isinstance(tail, int) or isinstance(tail, bool) or not 1 <= tail <= 1000:
        raise ReadError("tail must be an integer between 1 and 1000")
    info = read_run(client, namespace=namespace, name=name, kind=kind)
    if container not in info.containers.get(pod, []):
        raise ReadError("Pod/container membership could not be verified for this run.", 403)
    try:
        source = {**info.log_sources.get(pod, {}), "container": container}
        encoded = read_owned_logs(client, info, source, time.monotonic() + 10,
                                  tail=tail, previous=previous, timestamps=timestamps)
    except Exception as exc:
        raise ReadError("Could not read logs. Check pods/log permission and whether the selected current/previous container exists.", 502) from exc
    return {"pod": pod, "container": container, "text": encoded[:LOG_BYTES].decode("utf-8", errors="ignore"), "limitBytes": LOG_BYTES, "possiblyTruncated": True}


def record_log_source(info, pod):
    meta = metadata(pod)
    refs = [ref for ref in (_field(meta, "owner_references", "ownerReferences") or [])
            if _field(ref, "controller") is True]
    if len(refs) != 1 or not _field(meta, "uid"):
        return
    ref = refs[0]
    info.log_sources[_field(meta, "name")] = {
        "pod": _field(meta, "name"), "podUid": _field(meta, "uid"),
        "ownerKind": _field(ref, "kind"), "ownerUid": _field(ref, "uid"),
    }


def verify_log_source(client, info, source, deadline):
    if not info.uid or not source.get("podUid"):
        raise ValueError("Missing workload or pod identity")
    if info.kind == "RayJob":
        run = read_document(client.custom.get_namespaced_custom_object, deadline,
                            group="ray.io", version="v1", plural="rayjobs",
                            namespace=info.namespace, name=info.name)
    else:
        run = read_document(client.batch.read_namespaced_job, deadline,
                            namespace=info.namespace, name=info.name)
    if _field(metadata(run), "uid") != info.uid:
        raise ValueError("Workload identity changed")
    pod = read_document(client.core.read_namespaced_pod, deadline,
                        namespace=info.namespace, name=source["pod"])
    if (_field(metadata(pod), "uid") != source["podUid"]
            or not owned(pod, source["ownerKind"], source["ownerUid"])
            or source["container"] not in pod_containers(pod)):
        raise ValueError("Pod identity or container changed")
    if info.kind == "Job":
        if source["ownerKind"] != "Job" or source["ownerUid"] != info.uid:
            raise ValueError("Pod does not belong to this Job")
        return
    refs = [ref for ref in (_field(metadata(pod), "owner_references", "ownerReferences") or [])
            if _field(ref, "controller") is True and _field(ref, "uid") == source["ownerUid"]
            and _field(ref, "kind") == source["ownerKind"]]
    owner_name = _field(refs[0], "name") if len(refs) == 1 else None
    if not owner_name:
        raise ValueError("Pod controller name is missing")
    if source["ownerKind"] == "Job":
        controller = read_document(client.batch.read_namespaced_job, deadline,
                                   namespace=info.namespace, name=owner_name)
    elif source["ownerKind"] == "RayCluster":
        controller = read_document(client.custom.get_namespaced_custom_object, deadline,
                                   group="ray.io", version="v1", plural="rayclusters",
                                   namespace=info.namespace, name=owner_name)
    else:
        raise ValueError("Unsupported pod controller")
    if _field(metadata(controller), "uid") != source["ownerUid"] or not owned(controller, "RayJob", info.uid):
        raise ValueError("Pod controller identity changed")


def read_owned_logs(client, info, source, deadline, *, tail=1000, previous=False, timestamps=False):
    verify_log_source(client, info, source, deadline)
    response = client.core.read_namespaced_pod_log(
        namespace=info.namespace, name=source["pod"], container=source["container"],
        tail_lines=tail, previous=previous, timestamps=timestamps, follow=False,
        limit_bytes=LOG_BYTES + 1, _preload_content=False, _request_timeout=timeout(deadline))
    data = bounded_body(response, deadline)
    verify_log_source(client, info, source, deadline)
    return data
