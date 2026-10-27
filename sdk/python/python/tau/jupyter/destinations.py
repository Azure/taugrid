# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Bounded read-only choices for the native submission review."""

from tau._kube_io import read_document
from tau._notebook_submit import _read_tau_cluster
from tau._profile import destination_profiles
from tau.jupyter.runs import LIMIT, ReadError, list_namespaces, listing_items, truncated, validate_target

PROFILE_LIMIT = 200


def _visible(namespace, namespaces):
    """Validate one visible namespace name against a discovery listing.

    A name outside the listing is refused rather than trusted, so a request
    value never authorizes itself. This is the only place a name is accepted;
    it never selects a value.
    """
    validate_target(namespace)
    if not any(row["name"] == namespace for row in namespaces):
        raise ReadError("Namespace is not in the visible list. Refresh destinations or ask the operator for namespace read permission.")
    return namespace


def _pick_namespace(namespace, namespaces):
    """The one namespace the catalog reads, or ``None`` when nothing was named.

    A request that omits ``namespace`` resolves to ``None``: discovery never
    picks a destination for the user. The handler then authorizes no namespace
    and the picker returns every visible choice, so a denied default cannot hide
    the namespaces the caller is allowed to use.
    """
    if namespace:
        return _visible(namespace, namespaces)
    return None


def _resolved_namespace(namespace, namespaces):
    """Validate an already-resolved namespace against the listing it came from.

    Unlike :func:`_pick_namespace` this never selects a value: ``None`` stays
    ``None``, so a discovery that legitimately found no namespace keeps the
    catalog empty instead of a later call silently succeeding into one. A name
    is validated against the same snapshot it was resolved from, which keeps the
    authorized value and the read value identical.
    """
    return None if namespace is None else _visible(namespace, namespaces)


def resolve_destinations(client, namespace=None):
    """Resolve the namespace to authorize together with the listing it came from.

    This is the *only* namespace discovery in the destinations path. The
    returned namespace is the value the handler must authorize as
    ``taugrid:<namespace>`` before any profile or LocalQueue read; the returned
    snapshot is the same listing, handed to :func:`list_destinations` so the read
    never resolves independently. Discovery itself is a cluster-wide list, so the
    coarse ``taugrid`` grant covers this call.

    ``None`` is a real result: the request named no namespace, or discovery
    found none, so no namespaced read is authorized and the catalog stays
    scope-only. A namespace the request names is validated against this snapshot
    and refused when it is not visible.
    """
    discovery = list_namespaces(client)
    return _pick_namespace(namespace, discovery["namespaces"]), discovery


def list_destinations(client, namespace=None, *, team=None, lane=None, discovery=None):
    """Bounded, read-only destination choices for the native review.

    ``discovery`` is the snapshot ``namespace`` was already resolved from. When
    the handler supplies it (after authorizing the namespace it resolved), this
    function performs no namespace discovery of its own and treats ``namespace``
    as resolved: the read is bound to the value the authorization covered, and a
    resolved ``None`` keeps the catalog empty. Omitting it resolves standalone,
    which is only for direct callers with no authorization decision to bind to.

    ``team`` and ``lane`` are the caller scope the catalog is authorized
    against. They come from the server's own configuration and must be the same
    values the submission uses, so a profile offered here is the profile that
    can actually be submitted.
    """
    if discovery is None:
        discovery = list_namespaces(client)
        if namespace is None and discovery["namespaces"]:
            namespace = discovery["namespaces"][0]["name"]
        namespace = _pick_namespace(namespace, discovery["namespaces"])
    else:
        namespace = _resolved_namespace(namespace, discovery["namespaces"])
    warnings = list(discovery["warnings"])
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
