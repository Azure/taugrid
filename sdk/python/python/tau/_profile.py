# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Resolve ready, authorized worker profiles and a Kueue queue from a TauCluster.

Mirrors ``core/resourceprofile`` so the notebook plugin answers the same
question the CLI and the portal answer: *which profiles is this caller allowed
to use, right now, in this namespace?*

Two rules are load-bearing and both were previously wrong here:

1. **Readiness comes from the resolved status catalog**, not from the
   declaration. ``TauCluster.status.workloadProfiles`` is a
   ``ProfileSetStatus`` whose ``profiles[]`` entries embed the full
   ``WorkloadProfile`` inline, so the catalog is self-sufficient: sizing,
   applicability, priorities and queues all live there. ``spec`` is only
   *intent*; a profile present in spec but missing, stale, or not-ready in
   status must not be offered.

2. **Applicability is enforced before a profile is offered, and an unknown
   caller scope fails closed.** A profile authorizes a namespace, a team, and a
   lane. An empty list means "global"; a non-empty list must contain the
   caller's value. ``None`` means the caller scope does not identify that
   dimension, and an unknown value does *not* authorize a scoped profile, so a
   team-scoped profile is withheld when no team is known rather than silently
   offered. A caller scope value must come from a trusted, server-side source
   and must be the *same* value for discovery and submission; deriving it from
   the profile being authorized would be circular and is not an authorization
   boundary at all.

Everything here is a pure function of the TauCluster document, so tests pass a
plain dict and stay offline.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, List, Optional, Tuple

from tau._render import Profile, _worker_resources, default_image

DEFAULT_QUEUE_FALLBACK = "default"

#: The condition type the controller sets on a resolved profile. Mirrors
#: ``resourceprofile.ConditionReady``.
READY_CONDITION = "Ready"

#: Kubernetes condition statuses.
_CONDITION_TRUE = "True"
_APPLICABILITY_KEYS = frozenset({"namespaces", "teams", "lanes"})


class NoTauClusterFound(Exception):
    """No readable ``TauCluster/cluster`` document was supplied."""


@dataclass(frozen=True)
class SkippedProfile:
    """A profile that was declared but withheld, and why."""

    name: str
    reason: str


@dataclass(frozen=True)
class ProfileScope:
    """The caller scope an applicability check authorizes.

    Every dimension is fail-closed. ``None`` means the caller scope does not
    identify that dimension, so a profile that scopes on it is withheld; an
    explicit empty string is a known-empty value and likewise does not authorize
    a profile that scopes on it. Only an empty applicability list (global) passes
    a dimension whose caller value is unknown.

    The values here are an authorization input, so they must come from a trusted
    server-side source (for the notebook plugin, the Jupyter server's own
    configuration) and must be identical for discovery and submission. They must
    never be derived from the profile that is being authorized.
    """

    namespace: Optional[str] = None
    team: Optional[str] = None
    lane: Optional[str] = None

    def described(self) -> str:
        return (
            f"namespace={self.namespace or '<unknown>'}, "
            f"team={self.team or '<unknown>'}, "
            f"lane={self.lane or '<unknown>'}"
        )


# --------------------------------------------------------------------------
# Normalization — mirrors core/resourceprofile normalizeName/normalizeLabel.
# --------------------------------------------------------------------------


def normalize_name(value: Any) -> str:
    """Mirror ``resourceprofile.normalizeName``."""
    text = str(value or "").strip().lower().replace("_", "-").replace(" ", "-")
    return text.strip(".")


def normalize_label(value: Any) -> str:
    """Mirror ``resourceprofile.normalizeLabel``."""
    return str(value or "").strip().lower().replace("_", "-").replace(" ", "-")


def _normalize_list(values: Any, normalize) -> Tuple[str, ...]:
    if not isinstance(values, (list, tuple)) or not values:
        return ()
    return tuple(sorted({normalize(item) for item in values if str(item or "").strip()}))


def _normalize_scope_list(values: Any, normalize) -> Optional[Tuple[str, ...]]:
    if not isinstance(values, (list, tuple)):
        return None
    normalized = set()
    for item in values:
        if not isinstance(item, str):
            return None
        value = normalize(item)
        if not value:
            return None
        normalized.add(value)
    return tuple(sorted(normalized))


# --------------------------------------------------------------------------
# Readiness — mirrors provider.requireProfileReady / requireCondition.
# --------------------------------------------------------------------------


def _cluster_generation(cluster: Dict[str, Any]) -> int:
    metadata = cluster.get("metadata") or {}
    generation = metadata.get("generation")
    return generation if isinstance(generation, int) and not isinstance(generation, bool) else 0


def _ready_failure(profile: Dict[str, Any], generation: int) -> Optional[str]:
    """Return a reason string when the profile is not ready, else None."""
    conditions = profile.get("conditions") or []
    if not isinstance(conditions, list):
        conditions = []
    for condition in conditions:
        if not isinstance(condition, dict) or condition.get("type") != READY_CONDITION:
            continue
        observed = condition.get("observedGeneration")
        if observed != generation:
            return (
                f"condition {READY_CONDITION} is stale: observedGeneration "
                f"{observed!r} does not match generation {generation}"
            )
        if condition.get("status") != _CONDITION_TRUE:
            reason = str(condition.get("reason") or "").strip()
            message = str(condition.get("message") or "").strip()
            detail = ": ".join(part for part in (reason, message) if part)
            return f"condition {READY_CONDITION} is {condition.get('status')!r}" + (f" ({detail})" if detail else "")
        return None
    return f"missing condition {READY_CONDITION} at generation {generation}"


def _catalog(cluster: Dict[str, Any]) -> Tuple[int, List[Dict[str, Any]], Optional[str]]:
    """Return ``(generation, status profiles, blocking reason)``.

    ``blocking reason`` is set when the whole set is stale, which withholds
    every profile rather than offering a mix of current and stale ones.
    """
    generation = _cluster_generation(cluster)
    status = cluster.get("status") or {}
    section = status.get("workloadProfiles") or {}
    if not isinstance(section, dict):
        return generation, [], "status.workloadProfiles is not a mapping"
    observed = section.get("observedGeneration")
    if not isinstance(observed, int) or isinstance(observed, bool):
        return generation, [], (
            "status.workloadProfiles.observedGeneration is missing; the cluster "
            "has not published a resolved profile set"
        )
    if observed != generation:
        return generation, [], (
            f"workload profiles are stale: observedGeneration {observed} does not "
            f"match metadata.generation {generation}"
        )
    profiles = section.get("profiles")
    if not isinstance(profiles, list):
        return generation, [], "status.workloadProfiles.profiles is not a list"
    return generation, [p for p in profiles if isinstance(p, dict)], None


# --------------------------------------------------------------------------
# Declaration extras — fields the resolved status catalog cannot carry.
# --------------------------------------------------------------------------


def _declaration_extras(cluster: Dict[str, Any]) -> Dict[str, Dict[str, Any]]:
    """Optional scheduling fields keyed by profile name.

    ``WorkloadProfile`` models sizing, applicability, priorities and queues but
    has no ``cpusPerWorker``, ``memoryPerWorker``, ``nodeSelector`` or ``image``
    field. Those are read from the matching declaration when a document
    supplies them; readiness and authorization never come from here.
    """
    extras: Dict[str, Dict[str, Any]] = {}
    for item in (cluster.get("spec") or {}).get("workloadProfiles") or []:
        if isinstance(item, dict) and isinstance(item.get("name"), str):
            extras[str(item["name"])] = item
    return extras


def _optional_float(value: Any) -> Optional[float]:
    if value in (None, ""):
        return None
    number = float(value)
    if isinstance(value, bool):
        raise ValueError("cpusPerWorker must be a positive finite number")
    return number


def _optional_str(value: Any) -> Optional[str]:
    if value in (None, ""):
        return None
    return str(value)


def _resolve_profile(
    resolved: Dict[str, Any], declaration: Dict[str, Any]
) -> Profile:
    """Combine the authoritative resolved profile with optional declaration extras."""
    compute = resolved.get("compute") or {}
    spec_compute = declaration.get("compute") or {}
    workers = resolved.get(
        "workerCount", compute.get("workerReplicas", declaration.get("workerCount", 1))
    )
    gpus = resolved.get(
        "gpusPerWorker", compute.get("gpusPerWorker", declaration.get("gpusPerWorker", 0))
    )
    cpus = compute.get("cpusPerWorker", spec_compute.get("cpusPerWorker", declaration.get("cpusPerWorker")))
    image = compute.get("image", spec_compute.get("image", declaration.get("image")))
    memory = compute.get(
        "memoryPerWorker", spec_compute.get("memoryPerWorker", declaration.get("memoryPerWorker"))
    )
    selector = resolved.get("nodeSelector") or declaration.get("nodeSelector") or {}
    priorities = resolved.get("priorities") or {}
    spec_priorities = declaration.get("priorities") or {}
    priority = (
        priorities.get("podPriorityClassName")
        or resolved.get("priorityClass")
        or declaration.get("priorityClass")
        or spec_priorities.get("podPriorityClassName")
    )
    workload_priority = (
        priorities.get("workloadPriorityClassName")
        or spec_priorities.get("workloadPriorityClassName")
    )
    placement = resolved.get("placement") or declaration.get("placement")
    return Profile(
        name=str(resolved["name"]),
        workers=workers,
        gpus_per_worker=gpus,
        num_cpus_per_worker=_optional_float(cpus),
        priority_class=_optional_str(priority),
        workload_priority_class=_optional_str(workload_priority),
        image=_optional_str(image),
        node_selector=dict(selector) if selector else None,
        memory_per_worker=_optional_str(memory),
        placement=_optional_str(placement),
    )


def profile_queue(resolved: Dict[str, Any], namespace: Optional[str], fallback: str) -> str:
    """The Kueue LocalQueue a profile selects, in canonical order.

    Mirrors ``RenderProfile``: the profile's namespace mapping first, then the
    profile's own default, then the workspace fallback. The mapping must win:
    a GPU profile that reserves a quota path per namespace would otherwise be
    admitted against the workspace default and land in the wrong quota, or never
    be admitted at all.
    """
    wanted = normalize_name(namespace)
    for entry in resolved.get("localQueues") or []:
        if not isinstance(entry, dict):
            continue
        if normalize_name(entry.get("namespace")) == wanted:
            name = _optional_str(entry.get("name"))
            if name:
                return name
    default = _optional_str(resolved.get("defaultLocalQueue"))
    return default or fallback


# --------------------------------------------------------------------------
# Public surface
# --------------------------------------------------------------------------


def default_queue(cluster: Dict[str, Any]) -> Optional[str]:
    """Read ``spec.workspaceDefaults.defaultQueue`` if present."""
    spec = cluster.get("spec") or {}
    workspace_defaults = spec.get("workspaceDefaults") or {}
    return _optional_str(workspace_defaults.get("defaultQueue"))


def profile_provenance(cluster: Dict[str, Any]) -> Tuple[Optional[int], Optional[str]]:
    """``(cluster generation, profile-set hash)`` for the resolved catalog.

    Stamped on a rendered workload so the status/profile consumers can tell which
    resolved profile set authorized it. Either value may be absent on a document
    that predates the field; the caller stamps only what exists rather than
    inventing a generation.
    """
    generation = _cluster_generation(cluster)
    section = (cluster.get("status") or {}).get("workloadProfiles") or {}
    if not isinstance(section, dict):
        return (generation or None), None
    observed = section.get("observedGeneration")
    resolved_generation = (
        observed if isinstance(observed, int) and not isinstance(observed, bool) else generation
    )
    return (resolved_generation or None), _optional_str(section.get("profileSetHash"))


def ready_profiles(
    cluster: Dict[str, Any],
    *,
    scope: Optional[ProfileScope] = None,
) -> Tuple[List[Profile], List[SkippedProfile]]:
    """Ready and authorized profiles for ``scope``, plus what was withheld.

    Raises :class:`NoTauClusterFound` when ``cluster`` is empty/None so callers
    can distinguish "no cluster" from "no usable profiles".
    """
    if not cluster:
        raise NoTauClusterFound(
            "no TauCluster found: looked for the cluster-scoped resource "
            "TauCluster (group tau.azure.com, kind cluster)"
        )
    scope = scope or ProfileScope()
    generation, resolved, blocking = _catalog(cluster)
    if blocking is not None:
        return [], [SkippedProfile(name="<profile-set>", reason=blocking)]

    extras = _declaration_extras(cluster)

    profiles: List[Profile] = []
    skipped: List[SkippedProfile] = []
    for item in resolved:
        name = item.get("name")
        if not isinstance(name, str) or not name.strip():
            continue
        name = name.strip()
        failure = _ready_failure(item, generation)
        if failure is not None:
            skipped.append(SkippedProfile(name=name, reason=failure))
            continue
        applicability = item.get("applicability") if "applicability" in item else {}
        if not isinstance(applicability, dict):
            skipped.append(
                SkippedProfile(
                    name=name,
                    reason="malformed applicability: expected a mapping",
                )
            )
            continue
        unknown_keys = [key for key in applicability if key not in _APPLICABILITY_KEYS]
        if unknown_keys:
            skipped.append(
                SkippedProfile(
                    name=name,
                    reason=f"has malformed applicability: unknown key {unknown_keys[0]!r}",
                )
            )
            continue
        # Every dimension is checked, including one the caller scope does not
        # identify. An unknown caller value cannot select capacity a profile
        # reserves for a specific namespace, team, or lane: that is the
        # authorization boundary, and it must not be relaxed so a picker looks
        # fuller on a stock install.
        denied: Optional[str] = None
        for field, value, key, normalize in (
            ("namespace", scope.namespace, "namespaces", normalize_name),
            ("team", scope.team, "teams", normalize_label),
            ("lane", scope.lane, "lanes", normalize_label),
        ):
            if key not in applicability:
                continue
            allowed = _normalize_scope_list(applicability.get(key), normalize)
            if allowed is None:
                denied = f"has malformed applicability.{key}: expected a list"
                break
            if not allowed:
                continue  # an empty list is global
            if value is None:
                denied = (
                    f"does not authorize {field}: this profile is {field}-scoped and "
                    f"the caller scope does not identify a {field}"
                )
                break
            if normalize(value) not in allowed:
                denied = f"does not authorize {field} {normalize(value)!r}"
                break
        if denied is not None:
            skipped.append(SkippedProfile(name=name, reason=denied))
            continue
        try:
            profiles.append(_resolve_profile(item, extras.get(name, {})))
        except (ValueError, TypeError) as exc:
            skipped.append(SkippedProfile(name=name, reason=f"invalid profile: {exc}"))
    # Catalog order is preserved: it is the controller's resolved order, and the
    # first entry is what an unspecified profile resolves to.
    return profiles, skipped


def destination_profiles(
    cluster: Dict[str, Any],
    limit: int = 200,
    *,
    namespace: Optional[str] = None,
    team: Optional[str] = None,
    lane: Optional[str] = None,
) -> Dict[str, Any]:
    """A bounded catalog for the submission review, with withheld-profile reasons.

    Each row states whether the notebook's own kernel would receive a GPU under
    that profile. It never does: the notebook executes in the RayJob's
    control-only head. ``gpusPerWorker`` sizing applies to Ray *workers*, so a
    notebook that calls CUDA directly needs a different execution shape and the
    row says so instead of implying otherwise.
    """
    scope = ProfileScope(namespace=namespace, team=team, lane=lane)
    profiles, skipped = ready_profiles(cluster, scope=scope)
    fallback_queue = default_queue(cluster) or DEFAULT_QUEUE_FALLBACK
    descriptions = {
        str(item.get("name")): str(item.get("description") or "")[:1024]
        for item in (cluster.get("spec") or {}).get("workloadProfiles", [])
        if isinstance(item, dict) and item.get("name")
    }
    resolved_by_name = {
        str(item.get("name")): item
        for item in ((cluster.get("status") or {}).get("workloadProfiles") or {}).get("profiles", [])
        if isinstance(item, dict) and item.get("name")
    }
    rows = []
    for profile in profiles[:limit]:
        resources = _worker_resources(profile)
        # The head is control-only by contract, so the notebook's own kernel
        # never gets a GPU. Surface that as data rather than leaving the user to
        # infer it from a profile name.
        rows.append({
            "name": profile.name,
            "description": descriptions.get(profile.name, ""),
            "workers": profile.workers,
            "gpusPerWorker": profile.gpus_per_worker,
            "cpusPerWorker": resources["cpu"],
            "memoryPerWorker": resources["memory"],
            "priority": profile.priority_class,
            "image": profile.image or default_image(),
            "queue": profile_queue(resolved_by_name.get(profile.name, {}), namespace, fallback_queue),
            "notebookKernelGpu": False,
            "requiresRayDispatch": profile.gpus_per_worker > 0,
            "teams": _normalize_list(
                (resolved_by_name.get(profile.name, {}).get("applicability") or {}).get("teams"),
                normalize_label,
            ),
            "lanes": _normalize_list(
                (resolved_by_name.get(profile.name, {}).get("applicability") or {}).get("lanes"),
                normalize_label,
            ),
        })
    warnings = [
        f"Profile {item.name} is not offered in this scope: {item.reason}."
        for item in skipped
    ]
    return {
        "profiles": rows,
        "defaultQueue": fallback_queue,
        "profilesTruncated": len(profiles) > limit,
        "warnings": warnings,
        "scope": scope.described(),
    }


__all__ = [
    "Profile",
    "NoTauClusterFound",
    "ProfileScope",
    "SkippedProfile",
    "READY_CONDITION",
    "default_queue",
    "destination_profiles",
    "normalize_label",
    "normalize_name",
    "profile_provenance",
    "profile_queue",
    "ready_profiles",
    "DEFAULT_QUEUE_FALLBACK",
]
