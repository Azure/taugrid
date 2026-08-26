---
title: Configure Portal
linkTitle: Set up the Portal
weight: 50
description: Verify the default TauGrid Portal and configure additional Kubernetes and Kusto-backed capabilities
url: "/docs/platform-admin-guide/enable-portal/"
aliases:
  - "/docs/tasks/platform/enable-portal/"
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

Portal is the unified, read-only browser entry point. `tau cluster install` enables its operator-facing Kubernetes path by default in the system release namespace (`tau-system` unless `--namespace` selects another namespace); it ships as part of the one TauGrid umbrella release rather than a separate `taugrid-core` Helm release.

## Understand the default boundary

The default distribution creates `deployment/tau-portal`, `service/tau-portal`, a dedicated ServiceAccount, and cluster-wide read-only Kubernetes RBAC. Portal remains ClusterIP-only and relies on network-level access control rather than application-level login, so `kubectl port-forward` is an operator diagnostic rather than a researcher endpoint.

| Capability | Default state | Additional requirement |
|---|---|---|
| Portal shell, Runs, run detail, Cluster Nodes, live Ray discovery | Available through the default Kubernetes client and RBAC | A matching live workload or Ray head Service; Events remain a separate RBAC capability |
| Jobs / Queue computed board | Disabled | `portal.jobs.scopeMode` plus reviewed workspace-directory or operator scopes; workload profiles are read-only from ready TauCluster status |
| Kueue (Live) | Disabled | KueueViz Deployments/Services and `portal.kueueviz.enabled=true` |
| Experiments, Cluster Health, Cost | Degraded | ADX/Kusto endpoint, database, and a query identity |
| Durable Ray history | Disabled | Lifecycle recorder, successful schema management, and `portal.runHistory.enabled=true` |
| Researcher browser access | Not installed | Platform-owned authenticated HTTPS proxy, DNS, certificate, and reviewed network path |
| Services and Observability pages | Planned | No shipped backend yet |

The live Ray dashboard reflects present state only. Durable history remains available after KubeRay removes runtime Pods only when the lifecycle recorder and Kusto path are configured.

## Verify the default Portal

Install or upgrade TauGrid with the cluster's complete values file, then verify the Portal resource and health endpoint:

```bash
export TAU_SYSTEM_NAMESPACE=tau-system
tau cluster install --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" --values <platform-values.yaml>
kubectl --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" rollout status deployment/tau-portal --timeout=180s
kubectl --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" get service/tau-portal serviceaccount/tau-portal
kubectl --context <context> --namespace "$TAU_SYSTEM_NAMESPACE" port-forward service/tau-portal 18080:80
# In another terminal:
curl -fsS http://127.0.0.1:18080/healthz
```

The installation validation should include `PASS Portal`, the rollout should complete, the Service should be `ClusterIP`, and the health request should succeed. A default install can open `/portal` and Kubernetes-backed run details, but the capability table above remains the acceptance boundary.

## Confirm release capability before enabling ADX boards

Use a published TauGrid release whose chart and images include native Kusto Portal support; confirm the template contract for the exact release you use rather than assuming an arbitrary older release has the same one. The matching `taugrid-core` chart must accept `portal.runHistory.enabled` with `portal.kusto.endpoint` and no `portal.kusto.queryCommand`, and it must derive the `azure.workload.identity/use: "true"` Pod label from the Portal ServiceAccount's `azure.workload.identity/client-id` annotation.

Before changing a production release, render the exact published chart and review these conditions together with its pinned Portal image. If the release lacks them, use a newer published release instead of working around the gate with a nonexistent `queryCommand` or a manual Deployment patch.

## Keep one canonical TauGrid values file

The capability sections below describe keys in one desired-state document
rather than independent Helm overlays. `tau cluster install` resets Helm release values during its upgrade path, so a later invocation containing only a small Portal/Kusto fragment can reset other customized TauGrid components to chart defaults and cause Helm to remove resources that are no longer rendered. Keep the complete reviewed configuration for the cluster in `<platform-values.yaml>`, merge every enabled component into that file, and pass that full file on every upgrade. Portal has no separate namespace setting; it follows the TauGrid Helm release namespace.

Keep `taugrid-core` and the umbrella release mutually exclusive: installing
both would let each try to own Portal or recorder resources, creating
ambiguous Helm ownership.

## Scope the Kubernetes-backed boards

Create the target workspace first. The following is an intentionally non-standalone merge fragment for the canonical reviewed values file. The Portal enablement, ServiceAccount, and RBAC keys repeat distribution defaults so the desired state is explicit; preserve every existing top-level setting and every other enabled component in that file.

```text
# Merge into the existing <platform-values.yaml> as a partial fragment.
# Preserve components, baselineQueue, Kueue/KubeRay/controller settings, and
# every existing taugrid-core service configuration.
taugrid-core:
  portal:
    enabled: true
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
  --namespace "$TAU_SYSTEM_NAMESPACE" \
  --values <platform-values.yaml>
kubectl -n "$TAU_SYSTEM_NAMESPACE" rollout status deploy/tau-portal --timeout=180s
```

`portal.jobs.scopeMode` is disabled by default. Configure workspace-directory or explicit operator scopes before enabling it, so the Jobs board exposes only the intended workspace scope. `workloadNamespace` scopes the legacy Runs and Ray views only; `portal.jobs.scopeMode` separately authorizes the computed Jobs board.

## Add Kusto-backed boards to the same Portal block

First [prepare ADX/Kusto](../prepare-adx-kusto/). Merge the following keys into the existing `taugrid-core.portal` map above, keeping them there instead of a second values file passed alone to `tau cluster install`. A bare endpoint uses Portal's native `DefaultAzureCredential` path; `portal.kusto.queryCommand` is only an explicit adapter override and must exist in the image.

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

Durable Ray history is optional. Follow [Enable lifecycle recorder](../enable-lifecycle-recorder/) after adx-mon has successfully reconciled its lifecycle schema command and the writer identity is ready. Then merge the following field into the existing `portal` map:

```text
runHistory:
  enabled: true
```

Do this only after the recorder is successfully writing rows. This is an additional Portal capability layered on top of its core definition.

After the rollout, verify both the workload-identity injection and the durable history API before exposing Portal through an ingress:

```bash
kubectl -n "$TAU_SYSTEM_NAMESPACE" get pod -l app=tau-portal \
  -o jsonpath='{.items[0].metadata.labels.azure\.workload\.identity/use}{"\n"}'
kubectl -n "$TAU_SYSTEM_NAMESPACE" get pod -l app=tau-portal \
  -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="AZURE_FEDERATED_TOKEN_FILE")].value}{"\n"}'

kubectl -n "$TAU_SYSTEM_NAMESPACE" port-forward svc/tau-portal 18080:80
# In another terminal:
curl -fsS 'http://127.0.0.1:18080/api/portal/runs?workspace=<workspace-name>'
```

The API must report `"historyState":"available"` after the lifecycle recorder has ingested records. `history-unavailable` normally signals an ADX identity, network, schema, or release-template problem rather than evidence that Portal is reading durable history.

Portal Service is intentionally `ClusterIP`. Production access needs a platform-owned authenticated HTTPS proxy, DNS, certificate, and reviewed network path; `kubectl port-forward` is an operator diagnostic only.
