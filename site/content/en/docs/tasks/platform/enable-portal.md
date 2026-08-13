---
title: Enable Portal
weight: 5
description: Deploy the unified TauGrid Portal and select its Kubernetes and Kusto-backed capabilities
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

Portal is the unified, read-only browser entry point. It is part of the one
TauGrid umbrella release, not a separate `taugrid-core` Helm release.

| Capability | Data source | Requirement |
|---|---|---|
| Jobs / Queue, Cluster Nodes, live Ray | Kubernetes/Kueue; Ray is a live proxy | Portal Kubernetes RBAC; Ray head Service must exist |
| Experiments (embedded Stellar), Cluster Health, Cost | ADX/Kusto | Configured endpoint and a query identity |
| Durable Ray history | `Metrics.TauExpRunLifecycle` | Lifecycle recorder, schema, and `runHistory.enabled` |

The live Ray dashboard is not historical. Durable history remains available
after KubeRay removes runtime Pods.

## Keep one canonical TauGrid values file

The two capability sections below describe keys in one desired-state document;
they are **not** two independent Helm overlays. `tau cluster install` resets
Helm release values during its upgrade path, so a later invocation containing
only a small Portal/Kusto fragment can reset other previously customized
TauGrid components to chart defaults and cause Helm to remove resources that
are no longer rendered. Keep the complete reviewed configuration for the
cluster in `<platform-values.yaml>`, merge every enabled component into that
file, and pass that full file on every upgrade.

Do not install `taugrid-core` directly beside the umbrella release: both would
try to own Portal/recorder resources and create ambiguous Helm ownership.

## Enable Kubernetes-backed boards

Install TauGrid and create the target workspace first. The following is an
intentionally non-standalone **merge fragment** for the canonical reviewed
values file. Preserve every existing top-level setting and every other enabled
component in that file; do not copy this fragment into a new values file.

```text
# Merge into the existing <platform-values.yaml>; not a complete Helm values file.
# Preserve components, baselineQueue, Kueue/KubeRay/controller settings, and
# every existing taugrid-core service configuration.
taugrid-core:
  portal:
    enabled: true
    namespace: tau-system
    cluster: <aks-name>
    workspace: <workspace-name>
    workloadNamespace: <workspace-namespace>
    serviceAccount:
      create: true
      name: tau-portal
    rbac:
      create: true
```

```bash
tau cluster install --context <context> --version <taugrid-release-version> \
  --values <platform-values.yaml>
kubectl -n tau-system rollout status deploy/tau-portal --timeout=180s
```

`portal.jobs.scopeMode` is disabled by default. Configure workspace-directory
or explicit operator scopes before enabling it, so the Jobs board cannot expose
an unintended cross-workspace view.

## Add Kusto-backed boards to the same Portal block

First [prepare ADX/Kusto](../prepare-adx-kusto/). Merge the following keys into
the **existing** `taugrid-core.portal` map above; do not save it as a second
values file and invoke `tau cluster install` with that file alone. A bare endpoint uses Portal's native
`DefaultAzureCredential` path; `portal.kusto.queryCommand` is only an explicit
adapter override and must exist in the image.

```text
# Fields to merge into the portal object already shown above.
source: kusto
kusto:
  endpoint: https://<adx>.<region>.kusto.windows.net
  database: Metrics
serviceAccount:
  annotations:
    azure.workload.identity/client-id: <portal-query-identity-client-id>
```

Durable Ray history is optional. Follow [Enable lifecycle recorder](../enable-lifecycle-recorder/)
after its lifecycle schema and writer identity are ready. Then merge the
following field into the existing `portal` map:

```text
runHistory:
  enabled: true
```

Do this only after the recorder is successfully writing rows. This is an
additional Portal capability, not the definition of Portal itself.

Portal Service is intentionally `ClusterIP`. Production access needs a
platform-owned authenticated HTTPS proxy, DNS, certificate, and reviewed
network path; `kubectl port-forward` is an operator diagnostic only.
