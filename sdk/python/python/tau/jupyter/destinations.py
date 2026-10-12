# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded read-only choices for the native submission review."""

from tau._kube_io import read_document
from tau._notebook_submit import _read_tau_cluster
from tau._profile import destination_profiles
from tau.jupyter.runs import LIMIT, ReadError, list_namespaces, listing_items, truncated, validate_target

PROFILE_LIMIT = 200


def _pick_namespace(namespace, namespaces):
    """The one namespace the catalog reads, or ``None`` when none is visible.

    A request that omits ``namespace`` lets discovery pick the first visible
    namespace. The caller cannot know which one that is, so the Jupyter handler
    resolves it here (under the cluster-wide grant) and authorizes
    ``taugrid:<resolved>`` before this function's caller reads profiles and
    queues. A namespace outside the visible list is refused rather than trusted,
    so the request value never authorizes itself.
    """
    if namespace:
        validate_target(namespace)
        if not any(row["name"] == namespace for row in namespaces):
            raise ReadError("Namespace is not in the visible list. Refresh destinations or ask the operator for namespace read permission.")
        return namespace
    if namespaces:
        return namespaces[0]["name"]
    return None


def resolve_destination_namespace(client, namespace=None):
    """Resolve the namespace ``list_destinations`` would read, without reading it.

    Discovery is a cluster-wide list, so the coarse ``taugrid`` grant covers
    this. The returned namespace is the value the handler must authorize as
    ``taugrid:<namespace>`` before any profile or LocalQueue read.
    """
    return _pick_namespace(namespace, list_namespaces(client)["namespaces"])


def list_destinations(client, namespace=None, *, team=None, lane=None):
    """Bounded, read-only destination choices for the native review.

    ``team`` and ``lane`` are the caller scope the catalog is authorized
    against. They come from the server's own configuration and must be the same
    values the submission uses, so a profile offered here is the profile that
    can actually be submitted.
    """
    discovery = list_namespaces(client)
    namespaces = discovery["namespaces"]
    warnings = list(discovery["warnings"])
    namespace = _pick_namespace(namespace, namespaces)
    catalog = {"profiles": [], "defaultQueue": None}
    try:
        catalog = destination_profiles(
            _read_tau_cluster(client, None),
            PROFILE_LIMIT,
            namespace=namespace,
            team=team,
            lane=lane,
        )
        if catalog.pop("profilesTruncated"):
            warnings.append(f"Showing the first {PROFILE_LIMIT} resolved profiles; ask the operator to narrow the catalog if yours is missing.")
        warnings.extend(catalog.pop("warnings", []) or [])
    except Exception as exc:
        catalog.pop("warnings", None)
        warnings.append(f"Profiles could not be resolved. Ask the operator to publish ready profiles and grant TauCluster read permission, then refresh. Details: {exc}")
    queues = []
    if namespace:
        try:
            listing = read_document(client.custom.list_namespaced_custom_object,
                                    group="kueue.x-k8s.io", version="v1beta1", plural="localqueues",
                                    namespace=namespace, limit=LIMIT)
            items = listing_items(listing)
            queues = sorted({item["metadata"]["name"] for item in items[:LIMIT]
                             if isinstance(item, dict) and isinstance(item.get("metadata"), dict)
                             and isinstance(item["metadata"].get("name"), str)})
            if truncated(listing) or len(items) >= LIMIT:
                warnings.append(f"Queue discovery reached its {LIMIT}-queue bound; refresh or ask the operator if your queue is missing.")
        except Exception as exc:
            warnings.append(f"Queues could not be listed. Ask the operator for LocalQueue read permission and check the cluster connection, then refresh. Details: {exc}")
    if catalog["defaultQueue"] and catalog["defaultQueue"] not in queues:
        warnings.append(f"The resolved default queue {catalog['defaultQueue']} is not in this namespace's visible queue list. Select a visible queue or ask the operator to configure the default.")
    return {**discovery, **catalog, "namespace": namespace, "queues": queues,
            "warnings": warnings,
            "limits": {"namespaces": LIMIT, "profiles": PROFILE_LIMIT, "queues": LIMIT}}
