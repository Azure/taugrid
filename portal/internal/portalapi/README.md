# portalapi — unified observability portal

`portalapi` serves the runtime's single read-only web surface: the `/portal`
native React frontend and the `/api/portal/*` board APIs. Experiments use the
canonical `/api/v2/stellar` API on the same origin rather than embedding another
application.
It is started by `taugrid-portal portal serve` and, in-cluster, by a Deployment behind an
internal ClusterIP Service. It follows the persona-centered UI direction proposed in
[PR #827](https://github.com/Azure/taugrid/pull/827).

## Shape

- **Shell** — `assets/` is generated from `portal/frontend/` by `npm run build`.
  The Experiments tab opens directly on its native dashboard, without an
  overview or sidebar; Workloads and Platform retain their boards.
  Workspace, project, experiment, and run selection are
  URL-addressable.
  Shared overview links use `/portal?persona=platform` or `persona=workloads`;
  absent or invalid personas default to Workloads, independent of browser storage.
  Independent source panels expose Refresh/Retry, in-flight state, the last
  successful response time, and snapshot freshness. The 15-second stale time
  marks snapshots stale; it does not poll. Live embedded dashboards manage
  freshness inside their own UI instead of using snapshot controls.
- **Boards** — each `internal/portal/{cluster,cost,jobs,ray,nodes,runs,rdmavalidation}` package
  exposes `Board(ctx, source, Options) (Snapshot, error)`. Two data-source
  families back them: Kubernetes (Jobs/Ray/Nodes/Runs share one client-go
  `kubeclient` reader) and Kusto (Cluster/Cost/InfiniBand validation share a
  `kustoquery` querier).
- **Soft-degrade contract** — a handler with a nil data source returns **503**;
  a `Board()` error returns **502**; an empty-but-successful result is a normal
  **200**. Boards light up together per source family, so a portal without
  cluster access still serves the shell, Stellar, and the Kusto boards.
- **Workspace scope** — with a workspace directory configured, a trusted
  authenticated user/group identity is resolved to an authorized metadata-only
  `WorkspaceScope`. Kubernetes and workload-attributed Kusto reads enforce its
  cluster, namespace, and LocalQueue at the reader/query boundary and again when
  assembling Kusto rows. Workspace-scoped Stellar reads are Kusto-only and carry
  the authorized workspace through experiment, run, snapshot, and series
  requests; query parameters cannot replace either the workspace or a configured
  Kusto workspace boundary. Local scopes use local readers; remote scopes
  redirect only to a registered per-cluster Portal or return an explicit
  unavailable state.
- **Native experiments** — experiments, charts, launch metadata, and media use
  the same React application; there is no experiment iframe or persona sidebar.
  Managed workspaces may use a local `/stellar` path or an explicit HTTPS
  experiment endpoint, but `experimentsUrl` requires a Kusto-backed source.
  Portal permits only canonical workspace-aware read routes (experiment/run
  discovery, snapshots, series, capabilities, page assets, and exact-run
  artifact reads). `GET`/`HEAD` on `/api/v2/stellar/artifacts` and
  `/api/v2/stellar/artifact` (including the existing API aliases) require an
  explicit `target` resolving to one exact run in the authorized scope.
  Native previews and download links use the artifact's attributed `run_id`,
  not the experiment or group currently open in the dashboard. Missing or
  ambiguous ownership leaves metadata visible with an unavailable notice and
  makes no artifact request or download link.
  Experiment/group targets, conflicting target/run/artifact hints, foreign
  artifact IDs, mutations, status, and `/artifact/bundle/*` remain denied.
  The central `expapi.WorkspaceRouteAllowed` policy and
  `WithWorkspaceRoutePolicy` context remain authoritative for the local mount;
  standalone experiment API behavior is not restricted by this Portal policy.
  An optional trusted `experimentsBackend` serves remote native reads with
  server-pinned workspace/source, no browser credentials, no upstream redirects,
  and no local fallback. Its artifact reads enforce the same exact-run ownership.
  Native evidence does not show report cards, counts, or links.
  Stellar's handler hard-codes `X-Frame-Options:
  DENY`; the mount relaxes that blanket `DENY` to `SAMEORIGIN` while leaving
  stricter per-route headers untouched.

## Workspace directory contract

`taugrid-portal portal serve --workspace-directory=/path/workspaces.json` enables managed
scope mode. The file is strict JSON with `localCluster`, optional per-cluster
`endpoints`, and `workspaces`. Each workspace requires `id`, `cluster`,
`namespace`, `source`, and an `authorization` containing mode
`workspace-rbac` or `cluster-wide` plus at least one explicit user or group.
The resolved scope includes `authorizationMode` so the UX cannot present
cluster-wide authorization as workspace isolation. Infrastructure Kusto boards
use the whole resolved cluster for `cluster-wide`; `workspace-rbac` keeps
workload-attributed queries namespace-scoped.
The deployment must ensure the configured identity headers can only be written
by its Entra-aware authentication proxy. The Portal accepts at most 16 KiB and
256 entries in the group header (and 1 KiB in the user header); larger claims
fail closed with HTTP 431 instead of attempting partial authorization. Remote
Portal endpoints are server-configured HTTPS origins/base paths. Browser query
parameters cannot replace them and endpoint-like parameters are removed when a
request is redirected.

Without `--workspace-directory`, the Portal serves one workspace derived from
the `--workspace` CLI flag, defaulting to `default` when it is unset. Directory mode intentionally
does not load remote kubeconfigs or infer viewer access from the shared Portal
ServiceAccount. Local expstore and `source=auto` remain single-store tools rather
than multi-tenant boundaries; managed workspace experiment views force
`source=kusto`.

## Navigation and board APIs

The shell exposes **Experiments**, **Workloads**, and **Platform**. Selecting
Experiments opens its native dashboard directly.
"Tier" records how much of each proposal page is actually backed by data:

- **①** — frontend-only reuse of an existing board/API.
- **②** — a thin new board over data that already exists (this round: Runs).
- **③** — a net-new backend with no data source wired yet (deferred; see below).

| Tab | Page | Route | Backed by | Tier |
|---|---|---|---|---|
| Workloads | Jobs | `/portal/runs` | `runs.Board` → `/api/portal/runs` (batch Jobs + ray.io RayJobs) | ② |
| Workloads | Services | `/portal/services` | none — placeholder (Ray Serve / KServe) | ③ |
| Platform | Fleet | `/portal/fleet` | unified `nodes.Board` inventory, `cluster.Board` per-GPU telemetry, `nodeutil.Board` CPU/memory, continuous Node conditions, and `rdmavalidation.KustoReader` run evidence; exact validation detail uses workspace-scoped Stellar artifacts | ① + ② |
| Platform | Kueue | `/portal/jobs` | `jobs.Board` (Kueue queue snapshot) | ① (PriorityClass = ③) |
| Platform | Ray | `/portal/ray` | `ray.Board` (dashboard Services, K8s) | ① |
| Platform | Observability | `/portal/observability` | none — placeholder | ③ |
| Platform | Cost | `/portal/cost` | `cost.Board` (namespace GPU-hours, Kusto) | ① (per-user/team = ③) |
| Experiments | Native workspace | `/portal/experiments` | canonical `/api/v2/stellar` reads | ① |

Workloads and Platform retain their `/portal` overview landings; Experiments
goes directly to `/portal/experiments`, without a separate overview page.
The overview API remains available for existing board consumers.
Fleet capacity, GPU and node utilization, continuous health, and InfiniBand
evidence share one site-aware operational map. The legacy
`/portal/{cluster,gpu,nodes}` paths and old `?view=` links still resolve to the
unified Fleet page; an `instance` query focuses the inline per-GPU detail table.

## InfiniBand validation contract

Fleet InfiniBand status is point-in-time, run-based evidence, not continuous
network health. The summary card, cursor-paginated history, and
`/portal/fleet/infiniband/<validation-id>` detail route expose only **Passed**,
**Failed**, **Running**, **Unknown**, and **Stale**. Missing, partial, malformed,
unsupported, or unverified data never becomes Passed.

The existing experiment metrics path is the searchable index. The reader uses
the configured `--kusto-ingestion` shape: `TauExpMetrics` for projection
ingestion or `ExperimentMetrics` for repository-standard remote-write
ingestion. Each validation phase emits a workspace/cluster/namespace-scoped
`tau/run_status` marker with a versioned validation ID, schema, kind, and
lifecycle state. Terminal rows add the bounded `rdma_validation/*` scalars and
exact artifact URI/SHA linkage. The Portal selects the newest marker for each
validation by retry-safe
attempt/phase step, orders validations by event time plus validation ID, then
derives historical status only when the terminal status, lifecycle, reason,
namespace, and artifact linkage are internally consistent. Pagination cursors
are opaque and versioned. Workspace authorization and cluster scope are applied
before the read. The recorded diagnostic namespace is evidence, not an
authorization filter, because an authorized workspace validation can run in a
dedicated namespace that differs from its workload namespace.

The canonical `core/rdmavalidation.Result` JSON remains authoritative for
technical detail. The Portal never dereferences the metric's artifact URI.
After workspace authorization it lists the exact run's `rdma-validation`
artifact through Stellar, selects the contract-defined artifact ID/type/name and
digest, follows only Stellar's returned same-origin `fetch_url`, limits the
payload to 8 MiB, verifies size and SHA-256, requires canonical JSON with the
known schema, and runs the core fail-closed validation before returning detail.
Unknown schemas stay Unknown in history and return an explicit detail error;
malformed or integrity-failed artifacts also return typed errors rather than a
success-shaped fallback.

Historical pass/fail/unknown is immutable. Staleness is a separate presentation
state at the exact `valid_until` boundary; a result is fresh at `valid_until`
and stale only afterward. Message bytes are derived from element count only for
recognized fixed-width data types. Unknown data types leave message size
Unknown instead of guessing.

The Fleet InfiniBand view also reads the authorized Kubernetes node inventory
and conditionally renders an Unbounded-site-bounded GPU fabric map followed by
a row-oriented evidence matrix. The inventory reader selects the exact
canonical `unbounded-cloud.io/site` label first and the exact deprecated
`net.unbounded-cloud.io/site` migration label only when the canonical value is
empty. It never performs fuzzy label matching or substitutes region, zone, or
pool for site identity. If no GPU node has either supported label, the primary
map remains grouped by ordinary region and pool placement without presenting
an Unbounded site visualization. Partial coverage keeps every unlabeled GPU
node in an explicit Unknown bucket and reports the coverage gap.

Region, zone, and pool remain separate placement fields. The map shows each
node's inventory GPU count, and uses labeled color and shape cues to distinguish
RDMA scheduling capability, condition faults, unknown evidence, and the exact
GPU UUIDs sampled by the latest validation. It draws a validated run path only
for the artifact-recorded nodes and GPU UUIDs; sharing a site, pool, or
`rdma/*` resource never manufactures a connection claim.

It marks nodes as RDMA-advertised
only when their status exposes a positive `rdma/*` capacity or allocatable
resource, and shows Unbounded site, agent pool, region, and zone with the exact
label key selected by the inventory reader. The site label is a topology
boundary, not CNI health, and RDMA capability is not health.

Continuous GPU/NVLink and InfiniBand evidence comes from an explicit allowlist
of monitoring-owned Kubernetes Node condition families. A condition family is
**Observed OK** only when every required family appears exactly once with a
fresh `False` observation. A fresh `True` condition is **Fault** and takes
precedence over missing coverage in another family. Missing, duplicate,
malformed, stale, future-dated, or `Unknown` evidence stays **Unknown**. The
Portal uses a 15-minute freshness window and tolerates at most one minute of
future clock skew. Evaluation is keyed by condition type, so condition ordering
does not affect the result.

Per-GPU ADX telemetry remains a separate evidence column and currently reports
only whether every expected GPU has a complete row-remap verdict; it is not
presented as comprehensive GPU health. The latest run column remains
point-in-time two-GPU inter-node validation evidence. Same-site coverage is
reported only for nodes whose canonical validation artifact carries matching
tested-site evidence. Missing topology is Unknown; inventory region or zone is
never silently treated as a validation site. Each node links to the Fleet
Health view for the underlying per-GPU metrics.

## Data interpretation and recovery

`GET /api/portal/overview?view=workloads` returns profiles, queue counters, and
admitted-workload links without querying optional fleet, GPU, cost, or Ray
sources. The unqualified overview API retains its complete response.
Both overview personas use the admission-only request. Platform loads inventory
from `/api/portal/nodes`, health from `/api/portal/cluster`, and allocation cost
from `/api/portal/cost` in independent panels; slow telemetry cannot block
inventory or admission, and the UI does not duplicate aggregate overview reads.
Admission is quota reservation, not proof that pods are running: the workload
list and GPU reservations are explicitly admission-qualified.

Job details expose `diagnostics` for workloads, pods, events, and tracking:
`ready`, `empty`, `unavailable`, or `not_configured`. Indexed metrics can enable
a scoped Stellar link while a job is active; a terminal lifecycle marker is not
required. HTTP 200 responses with unavailable workloads, pods, or events retain
the last successful section within the same authorized query and job incarnation,
with a stale warning and its original section-success time. A successful empty
section clears old rows. Tracking, lifecycle, and links always come from the
latest response; they are not merged from previous snapshots. Retried source
failures do not silently erase retained evidence or label it fresh.
Explicit client/access rejections (including 401,
403, and authorization-masked 404) hide cached board and native experiment data,
including derived selections and media previews, even during retries; only
transient/network and server failures retain data with a last-success stale warning.
Rejected cached payloads are discarded; later failures or cancellation cannot
restore them. Only a subsequent successful fetch restores readable data.
A failed workspace
directory refresh never displays a cached authorized directory or workspace.

Missing GPU and node measurements are JSON `null`, not measured zero. GPU
health requires observed error counters. Node CPU rates use actual observed
per-core intervals, exclude resets, and report sample/time coverage. Bounded
sample-transfer limits fail explicitly instead of computing a rate from a
silently truncated series. `queriedAt` records query completion, not the age of
every underlying sample.

Cost remains allocation-based, using schema-v4 `GpuCostHourly` rows.
`costAvailable` and `gpuHoursAvailable` distinguish unknown totals from measured
zero; their coverage counters expose partial sums. Raw GPU utilization is a
separate efficiency signal. `idleAvailable` requires enough valid readings to
assess at least one GPU. An empty idle list is not an all-clear for unobserved
hardware.

## Ray dashboard proxy

Every dashboard request carries its namespace and cluster in the target-prefixed
URL. Managed entries redirect to a prefix that also encodes the authorized
workspace. No origin-wide selected-target cookie routes traffic between tabs.
Only a target-specific Ray authentication cookie is forwarded to its upstream.

The proxy follows the relative-URL contract of official Ray 2.54/2.56 dashboard
builds: it rewrites structural HTML URL attributes and the document base, not
JavaScript source. Relative APIs, assets, and log-tail WebSockets retain their
target. Custom builds with incompatible absolute URLs are not supported.

Portal permits passive dashboard reads and the native authentication exchange.
Mutation, active profiling/traceback/JVM diagnostics, and dataset routes that
create a stats actor are unavailable through this read-only proxy. Unsupported
routes return an explicit response instead of selecting another dashboard or
redirecting users into an authentication loop.

## Deferred backends (③)

Each of these is a page or field the proposal draws but the portal has **no data
source for today**. They are listed here — not implemented — so the gap between
"the mock renders it" and "the runtime emits it" stays explicit.

1. **Cost per-user/team + budgets.** Workspace/namespace allocation chargeback
   exists. Per-user attribution and budget burn still need their own identity,
   budget, and reporting contracts; utilization alone cannot supply them.

2. **Fleet depth — NPD and AlertRule.** The unified Fleet map now reads
   continuous GPU, NVLink, and InfiniBand Node conditions independently from
   run validation evidence. Two richer signals remain unavailable:
   - **NPD** DaemonSet health would need a Kubernetes read of NPD pods/conditions.
   - **AlertRule** evaluation would need to read adx-mon AlertRule CRDs and their
     firing state.

3. **Observability page.** `/portal/observability` is a placeholder. A real board
   must read the adx-mon `Collector` / `Ingestor` / `Alerter` / `AlertRule` CRDs,
   pod readiness, and ingestor WAL metrics — all internal to adx-mon and not
   currently exposed to the portal.

4. **GPU Utilization heatmap / per-team**, **Kueue PriorityClass detail**,
   **sidebar user/team identity**, and **in-cluster RBAC**. The util view is a
   ranked list over the existing Cluster API; a heatmap or per-team split needs a
   time-series/grouping source. Kueue PriorityClass detail needs a richer queue
   snapshot. A sidebar "user/team" footer needs a user identity in front of the
   portal (it runs behind a shared read-only ServiceAccount with no per-request
   identity). Serving Runs in-cluster also needs the portal's ServiceAccount
   granted read on `batch/jobs` and `ray.io/rayjobs`.

Each item names the missing data source so a future increment knows where to
start — data that is genuinely present versus a design that merely draws it.
