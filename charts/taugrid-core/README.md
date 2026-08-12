# taugrid-core

The first-party Tau-owned bits of a TauGrid-on-AKS install.

Wraps the resources we wrote and want to ship as one helm release:

1. **Tau Stellar** — optional always-on experiment UI for test clusters. When
   enabled, it serves `taugrid-portal experiment serve --source=kusto` behind
   `svc/tau-stellar` on system nodes only. The deployed Kusto path is stateless
   and read-only; local expstore/PVC metric mode remains manual-only.
2. **Tau portal** — optional unified read-only observability portal. When
   enabled, it serves `taugrid-portal portal serve` behind `svc/tau-portal`, aggregating
   and cross-linking the runtime's dashboards (Experiments, Jobs/Queue, Cluster
   Health, Cluster Nodes, Ray, Cost) at `/portal`. Stellar is embedded as the
   Experiments board. See [Tau portal](#tau-portal) below.
3. **Image prewarm** — DaemonSet that pulls Tau's default GPU workload image on
   every GPU node so the first job on a fresh node skips the cold pull. It only
   helps when researchers submit without overriding `runtime.image`; workloads
   that build their own image want a peer-to-peer mirror instead (see Spegel in
   [`../INSTALL.md`](../INSTALL.md)).
4. **Resource flavors + namespaces** — optional static glue for clusters that
   want the chart to own those objects.
5. **Tau run lifecycle recorder** — optional, metadata-only projection of
   Jobs, RayJobs, and Kueue Workloads into ADX. It is disabled by default and
   requires explicit identity, endpoint, retention, and workload-TTL planning.

> `tau-core-controller` reconciles GPU topology labels from the standard
> `node.kubernetes.io/instance-type` label. Its Node watch covers both
> AKS-managed and AKS Flex nodes, including nodes that join after installation.
> The Tau CLI never writes Node labels.

## GPU class ResourceFlavors

`gpu_class` is a stable hardware-only API with three canonical values:
`a100-80gb`, `h100-95gb`, and `h200-141gb`. `any` is explicit unconstrained
selection. For a specific class, Tau renders
`tau.azure.com/gpu-class=<class>` and Kueue must see the same exact value in the
selected ResourceFlavor's `spec.nodeLabels` and on matching GPU nodes.
ResourceFlavor names such as `ndm-a100-v4` and `nd-h200-v5` remain
platform-owned identifiers; Tau never infers a class from those names.

```yaml
resourceFlavors:
  enabled: true
  items:
    - name: nd-h200-v5
      nodeLabels:
        tau.azure.com/gpu-class: h200-141gb
      topologyName: default-node-topology
      requiredTopology: kubernetes.io/hostname
```

Keep placement/interconnect in workload topology (`independent`,
`single-node-nvlink`, `multi-node-nccl`, or `elastic-workers`), not in the
class label. The sibling `tau-core-controller` chart continuously derives class
and series labels for its reviewed AKS GPU VM-size catalog. Install an
equivalent node-label reconciler when deploying this services chart alone.
For TAS-only flavors, `requiredTopology` renders the ResourceFlavor metadata
annotation Tau uses to make generated workloads admissible without a hidden
user-side annotation. Expert-authored raw manifests are not modified.

## What this chart does NOT install

This chart deliberately does not vendor or wrap upstream OSS charts.
Each one has its own opinionated values surface (taints, MIG profiles,
time-slicing, federated identity, cloud creds) we don't want to flatten
through a single values file. See [`../INSTALL.md`](../INSTALL.md) for
the recommended OSS install order + pinned versions.

It also no longer installs the old cost oracle, PrometheusRule, or Grafana
dashboard. Those resources belonged to the removed `tau cost`/pricing surface,
not to the current vNext deploy contract.

## Install

```bash
# Assumes Kueue + KubeRay are installed and GPU nodes expose nvidia.com/gpu.
# See ../INSTALL.md.
helm upgrade --install taugrid-core ./taugrid-core --kube-context my-cluster
```

### Namespace ownership

`namespaces.create` (default `true`) covers only the namespaces this chart's own
components render into: `prewarm.namespace`, `stellar.namespace`,
`portal.namespace`, and `lifecycleRecorder.namespace`.

Helm refuses to apply over an object it did not create. A namespace that already
exists out-of-band — from platform queue policy, from workspace tooling, or
from a plain `kubectl create namespace` — has none of Helm's ownership metadata,
so a chart that rendered it would fail install with:

```
Error: Unable to continue with install: Namespace "<name>" exists and cannot be
imported into the current release: invalid ownership metadata; label validation
error: missing key "app.kubernetes.io/managed-by": must be set to "Helm" ...
```

The chart therefore renders a Namespace only when it is absent or already
carries *this* release's ownership metadata, and skips one owned by anything
else. Install and upgrade both succeed against a cluster where the namespace
already exists; `namespaces.create: false` remains available for GitOps overlays
that own the whole set.

A skipped namespace keeps whatever labels it has. This chart does not reconcile
its Pod Security Standards labels, because it does not own it. To hand one over
deliberately, stamp the three keys Helm checks and upgrade — the next render
adopts it and starts reconciling those labels:

```bash
kubectl label namespace <name> app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate namespace <name> \
  meta.helm.sh/release-name=<release> \
  meta.helm.sh/release-namespace=<release-namespace> --overwrite
```

A namespace created by this chart under a *former* release name counts as owned
by something else, so it is skipped after a release rename. Adopt it with the
same command, or keep the release name (see above).

`lifecycleRecorder.targetNamespace` is never created here. It is the observed
workload namespace, owned by `tau-core-controller` (from a `TauWorkspace`) or by
platform queue policy. Enabling the recorder against a namespace that does
not exist fails with a message naming the value and those owners, instead of
surfacing later as `namespaces "<name>" not found` on the recorder's Role.

### Upgrading existing releases

The chart package and directory are now `taugrid-core`, but existing clusters
must keep their current Helm release name during this first rename step. For
example, upgrade an existing `taugrid-core` release with:

```bash
helm upgrade --install taugrid-core ./taugrid-core --kube-context my-cluster
```

Changing the release name to `taugrid-core` on an existing cluster would install
a second release instead of upgrading the current one. The checked-in Portal
GitOps overlays use `releaseName: taugrid-portal` so their ArgoCD and Helm
identities match the product name. Those overlays also set
`portal.resourceName` and `portal.serviceName` to `taugrid-portal`. The
non-overlapping names let the new Application create its Deployment and Service
without patching the former Deployment's immutable selector; the retired
Application can then prune its resources independently. Reverting the GitOps
rename restores the former Application and resource names with the same pinned
image. The former standalone Stellar GitOps application was retired; manually
managed `taugrid-core` releases must still keep that release name when upgrading
to this chart path. The ADO pipeline also
intentionally keeps its externally registered filename
`the release pipeline`. This chart now renders `tau-*` object
names in the `tau` namespace for both the portal and Stellar. It does not adopt
the legacy `tau-*` Services and PVCs still running on the test clusters: those
were installed by superseded chart generations and need a separate
decommission. Enabling Stellar here requires explicitly named existing
platform-managed claims; it neither adopts the old volumes nor provisions new
ones. Standard Helm chart
identity labels (`app.kubernetes.io/name` and `helm.sh/chart`) follow the new
package name and version.

Standalone Stellar is disabled in both generic and test-cluster defaults because
the hosted experience now runs through Tau Portal. The retained values are an
explicit compatibility/debug path in the `tau` namespace; keep that namespace
distinct from the `ray` workload namespace used for researcher RayJobs, Kueue
LocalQueues, and the `blob-training` `/data` PVC. To render the deprecated
standalone path, opt in explicitly. The chart defaults to the TauGrid Portal
MCR release tag matching the chart version:

```bash
# Keep the existing taugrid-core release name for this test-cluster upgrade.
helm upgrade --install taugrid-core ./taugrid-core \
  --kube-context my-cluster \
  -f values-test-clusters.yaml \
  --set stellar.enabled=true \
  --set stellar.kusto.queryCommand=/usr/local/bin/query-kusto \
  --set stellar.serviceAccount.create=true \
  --set stellar.serviceAccount.name=tau-stellar
```

When explicitly enabled, the compatibility values schedule the pod with
`nodeSelector: kubernetes.azure.com/mode=system`, keep `svc/tau-stellar`
ClusterIP-only, and read scalar dashboard rows from ADX/Kusto. It does not mount
an expstore or workload `blob-training` PVC while `stellar.source=kusto`, and it
has no writable overlay backend. Standalone Stellar has no application-level
login and is an operator-debug compatibility path, not the researcher-facing
acceptance surface. The chart rejects a LoadBalancer; use the unified Portal
access contract below for supported browser access.

By default enabled Stellar uses `stellar.source=kusto`: training emits
`experiment_metrics` remote-write samples, adx-mon ingests them into
`Metrics.ExperimentMetrics`, and Stellar queries ADX/Kusto for scalar dashboards.
adx-mon is the ingest path only; it is not the dashboard query source.
The canonical telemetry naming/versioning contract is documented in
[`../../../../docs/tau/tau-telemetry-schema-versioning.md`](../../../../docs/tau/tau-telemetry-schema-versioning.md).

`stellar.source=kusto` requires either `stellar.kusto.metricsFile` for a manual
export or `stellar.kusto.queryCommand` for live ADX queries. The query command
must be present in the Tau image or supplied by the cluster, have identity/RBAC
to query ADX, and emit Stellar row JSON/JSONL or Kusto REST JSON. Set
`stellar.kusto.ingestion=remote-write` for the adx-mon `ExperimentMetrics` table.
Set `stellar.serviceAccount` plus `stellar.podLabels` for a dedicated query
identity, such as Azure Workload Identity, and grant that identity the ADX
database viewer role on the database that contains `Metrics.ExperimentMetrics`.
Use `stellar.extraInitContainers`, `stellar.extraVolumes`, and
`stellar.extraVolumeMounts` to mount a query adapter binary or projected
credentials into the main Stellar container without enabling local expstore
imports.

Hosted Stellar discovers experiments across all Tau/W&B project labels visible
to the query identity by default. Landing/search discovery uses
`stellar.discovery.since` (default `90d`) with `stellar.discovery.maxSince`
(default `365d`) as the unscoped safety cap; targeted dashboard URLs use
`stellar.kusto.targetSince` (default `365d`). To hard-scope a team deployment,
set `stellar.discovery.allowedProjects`; do not use the deprecated
`stellar.kusto.project` as a Kusto-level filter. For example, RAD-DINO rows under
`project=rad-dino-chexpert` are visible from the shared dashboard without
redeploying a `tau-submit`-scoped Stellar. Add `?project=rad-dino-chexpert` to a
URL when you want a request-level filter.

Use `stellar.source=local` or `stellar.source=auto` only for explicit
debug/offline recovery. Those modes require the existing platform-managed claim
named by `stellar.expstore.pvcName` and can enable the manual importer sidecar;
`source=kusto` intentionally rejects the importer so the current telemetry path
has one scalar source of truth. This chart never creates or deletes either
claim.

## Verify

```bash
kubectl --context my-cluster -n tau rollout status deployment/tau-stellar
kubectl --context my-cluster -n tau get svc tau-stellar
```

## Tau run lifecycle recorder

`lifecycleRecorder` is a single-replica, metadata-only
`tau run history record` Deployment for an operator-owned workload namespace.
It is disabled by default and deliberately has no test-cluster overlay or
identity defaults. Do not enable it on shared clusters until its dedicated
ingestion identity, namespace scope, and recovery/TTL behavior have been
reviewed.

The chart defaults to the MCR Tau release tag matching the chart version and
also accepts an immutable digest override when `lifecycleRecorder.image.tag` is
set to an empty string. It additionally requires a target namespace, cluster
label, ADX endpoint, a chart-created dedicated ServiceAccount, and chart-created
least-privilege RBAC. It creates a namespace-scoped Role granting only
`get/list/watch` on Jobs, RayJobs, and Kueue Workloads in that target namespace.
Workload Identity is mandatory; it requires a ServiceAccount client-id annotation
and stamps the AKS workload-identity pod label.

Keep `portal.runHistory.enabled=false` until this recorder is deployed and
successfully writing lifecycle rows. The Portal then reports Runs as `live-only`
instead of claiming that an empty but queryable table is durable history.
Enable both components together after the recorder contract is satisfied.

For the identity, retention, failure semantics, and secure deployment contract,
see [Tau run lifecycle recorder](../../../../docs/tau/tau-run-lifecycle-recorder.md).

## Tau portal

The portal is a single read-only web entry point that aggregates and
cross-links the runtime's dashboards. It is the same `tau` binary and image as
Stellar, run as `taugrid-portal portal serve`, and embeds Stellar unchanged as the
Experiments board under `/stellar`. It is disabled by default; enable it
alongside or instead of the standalone Stellar Deployment.

```bash
helm upgrade --install taugrid-core ./taugrid-core \
  --kube-context my-cluster \
  --set portal.enabled=true \
  --set portal.serviceAccount.create=true \
  --set portal.serviceAccount.name=tau-portal \
  --set portal.rbac.create=true \
  --set portal.kusto.queryCommand=/usr/local/bin/query-kusto \
  --set portal.kusto.endpoint=<adx-endpoint> \
  --set portal.kusto.database=Metrics
```

The portal serves in the `tau` namespace behind a ClusterIP-only
`svc/tau-portal`. Portal has no application-level login, so the chart rejects
`LoadBalancer` and requires a platform-owned authenticated HTTPS proxy for
researcher browser access. Each board degrades independently, so a partial
install still serves the rest:

- **Experiments / Cluster Health / Cost** read ADX/Kusto through the shared
  `portal.kusto.queryCommand` (the same shell-out contract as Stellar). Without
  it, `source=kusto` fails the render; the Cluster Health and Cost board APIs
  return 503 when the command is absent at runtime.
- **Jobs / Queue, Ray, and Cluster Nodes** read the Kubernetes API directly with
  the pod's ServiceAccount. The computed Jobs board is disabled by default; use
  `portal.jobs.scopeMode=workspace` with an authenticated workspace directory,
  or `operator` with explicit `{team, namespace, localQueue}` entries. Set
  `portal.rbac.create=true` (which requires
  `portal.serviceAccount.create=true`) to create a ClusterRole granting read
  access to core `services` (Ray dashboard discovery), core `nodes` (Cluster
  Nodes hardware inventory), and Kueue `localqueues`/`clusterqueues`/`workloads`
  (queue depth). Without the RBAC, those boards return 502 with the API server's
  forbidden error and the rest of the portal still serves; 503 is reserved for a
  portal that could not build a Kubernetes client at all.

`portal.jobs.scopeMode` is omitted from the rendered command by default, and
Portal binaries default the board to `disabled`, so the board stays off until an
operator sets a mode. Operator activation also requires a topology policy whose
ClusterQueue binding matches each live LocalQueue; mismatches fail unavailable
instead of showing zero quota.

With `portal.workspaceDirectory.enabled=false` (the default), `portal.workspace`
(default `default`) scopes the embedded Stellar board. The legacy Ray/Runs
boards read cluster-wide unless `portal.workloadNamespace` is set; the
deprecated `portal.jobs.namespace` remains an explicit compatibility fallback.
It does not authorize the Jobs board. Viewer-authorized Jobs access requires
`portal.jobs.scopeMode=workspace`, team-qualified workspace records, and a
trusted authentication proxy supplying the configured identity headers.
Trusted test-cluster operator deployments can instead use `operator` mode with
explicit scopes. The chart mounts only metadata; it never mounts remote
kubeconfigs. Grant the portal's ServiceAccount the ADX database viewer role
(e.g. via Azure Workload Identity on `portal.serviceAccount` +
`portal.podLabels`) so `queryCommand` can reach ADX.

### Researcher browser access

This chart does not install an ingress controller, Gateway, DNS record,
certificate, or authentication proxy. Those resources are platform-owned
because their identity, network, and certificate policies vary by environment.
A supported researcher endpoint has all of these properties:

1. A stable HTTPS URL and trusted certificate.
2. Entra-aware authentication before any Portal route is served.
3. Private or otherwise reviewed network reachability, with no direct path that
   bypasses the proxy and reaches the ClusterIP backend.
4. Identity headers stripped from browser requests and replaced with claims
   produced by the trusted proxy. This is mandatory when
   `portal.workspaceDirectory` consumes `X-MS-CLIENT-PRINCIPAL-*`.
5. The overlay records `portal.access.mode=authenticated-proxy` and the exact
   browser URL in `portal.access.externalURL`.

The access values declare the deployment contract and fail unsafe chart
configurations; they do not provision or probe the external proxy.

Until that platform path exists, keep
`portal.access.mode=cluster-internal` and leave `externalURL` empty. In that
state there is intentionally no supported researcher browser signoff.
`kubectl port-forward` is an operator diagnostic only and must not be used as
researcher acceptance evidence. Historical IP addresses are not an endpoint
contract.

Browser signoff uses only the declared URL:

```text
https://<platform-owned-host>/portal
```

The researcher must be challenged for identity (or use an existing authenticated
browser session), reach the expected workspace and namespace-scoped data, and
open proxied boards without a kubeconfig, raw `kubectl`, or port-forward.

The embedded **Ray** board is a transport proxy to the actual Ray dashboard.
Portal access does not install Prometheus or Grafana and does not make Ray's
optional metrics panels available. Missing Prometheus/Grafana integration is a
separate Ray observability deployment prerequisite; track and validate it
separately when those panels are part of acceptance.

### Verify (portal, platform operator)

```bash
kubectl --context my-cluster -n tau rollout status deployment/tau-portal
kubectl --context my-cluster -n tau get svc tau-portal
```
