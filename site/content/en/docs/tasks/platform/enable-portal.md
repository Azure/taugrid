---
title: Enable Portal
weight: 5
description: Deploy the unified TauGrid Portal and select its Kubernetes and Kusto-backed capabilities
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

Portal is the unified, read-only browser entry point. It is part of the one
TauGrid umbrella release, not a separate `taugrid-core` Helm release.

## Confirm release capability before enabling ADX boards

This page describes the Portal and chart contract together. Use a published
TauGrid release whose **chart and images** include native Kusto Portal support;
do not assume that an arbitrary older `<taugrid-release-version>` has the same
template contract. In particular, the matching `taugrid-core` chart must:

- accept `portal.runHistory.enabled` with `portal.kusto.endpoint` and no
  `portal.kusto.queryCommand`; and
- derive the `azure.workload.identity/use: "true"` Pod label from the Portal
  ServiceAccount's `azure.workload.identity/client-id` annotation.

Before changing a production release, render the exact published chart and
review these conditions together with its pinned Portal image. If the release
does not have them, use a newer published release; do not work around the gate
with a nonexistent `queryCommand` or manually patch the Deployment.

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
values file and invoke `tau cluster install` with that file alone. A bare
endpoint uses Portal's native `DefaultAzureCredential` path;
`portal.kusto.queryCommand` is only an explicit adapter override and must
exist in the image.

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

After the rollout, verify both the workload-identity injection and the durable
history API before exposing Portal through an ingress:

```bash
kubectl -n tau-system get pod -l app=tau-portal \
  -o jsonpath='{.items[0].metadata.labels.azure\.workload\.identity/use}{"\n"}'
kubectl -n tau-system get pod -l app=tau-portal \
  -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="AZURE_FEDERATED_TOKEN_FILE")].value}{"\n"}'

kubectl -n tau-system port-forward svc/tau-portal 18080:80
# In another terminal:
curl -fsS 'http://127.0.0.1:18080/api/portal/runs?workspace=<workspace-name>'
```

The API must report `"historyState":"available"` after the lifecycle
recorder has ingested records. `history-unavailable` normally means an ADX
identity, network, schema, or release-template problem; it is not evidence
that Portal is reading durable history.

Portal Service is intentionally `ClusterIP`. Production access needs a
platform-owned authenticated HTTPS proxy, DNS, certificate, and reviewed
network path; `kubectl port-forward` is an operator diagnostic only.
