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
- **Boards** — each `internal/portal/{cluster,cost,jobs,ray,nodes,runs}` package
  exposes `Board(ctx, source, Options) (Snapshot, error)`. Two data-source
  families back them: Kubernetes (Jobs/Ray/Nodes/Runs share one client-go
  `kubeclient` reader) and Kusto (Cluster/Cost/Node Utilization share a `kustoquery` querier).
- **Soft-degrade contract** — after authorization, Fleet requests
  with a nil data source return **503**; upstream or sample-integrity failures
  return **502**; an empty-but-successful result is a normal
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
| Platform | Fleet | `/portal/fleet` | unified `nodes.Board` inventory, `cluster.Board` per-GPU telemetry, `nodeutil.Board` CPU/memory, and continuous Node conditions | ① + ② |
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

## Fleet InfiniBand evidence

The Fleet page combines authorized Kubernetes node inventory and current
Metrics Server CPU/memory usage, per-GPU ADX telemetry, recent ADX node
utilization, and continuous GPU/NVLink and InfiniBand Node conditions in one
operational dashboard. Inventory, telemetry, and utilization refresh and retry
together while retaining independent freshness and failure status. Current
Metrics Server samples are preferred on exact inventory nodes; exact ADX node
rows are a fallback, while mismatched telemetry remains visible as independent
source evidence.

Fleet remains a current operational snapshot view, without historical range
controls. Temporal bookmark parameters do not change its data reads or refresh;
the legacy Fleet route aliases and focused-node links remain supported. GPU and
ADX node utilization use the backend-default **15-minute** lookback. This is not
a historical reconstruction of inventory, Metrics Server samples or Node health.

For direct API clients, `/api/portal/cluster` and `/api/portal/nodeutil` retain
their original relative `window` contract: positive Go durations (including
values above one hour) select a lookback; missing, malformed or nonpositive
values fall back to 15 minutes. Repeated `window` values use the first value;
unknown `start`/`end` parameters are ignored. The unmerged Fleet absolute-history
expansion and temporary one-hour policy have been withdrawn. There is no new
Fleet migration or one-hour limit. Non-Fleet history retains its 30-day contract.
The raw CPU sample limits (250000 total and 4096 per core), reset handling and
truncation checks remain intact: even a default-window query can exceed a
sample budget on a large fleet and return **502**. Relative-window compatibility
is not a large-fleet capacity guarantee. No data migration is needed.

Experiment run-search fallback is restricted to the same historical data range.
Changing range hides previous-range results while loading or after a failure,
without resetting hidden-run selections, page size or other workspace state.
Same-range pagination failures retain the last successful page for retry;
display-timezone changes do not change data identity.

Custom Apply preserves each untouched RFC3339 bound verbatim, including offsets
and nanoseconds. Editing one bound changes only that bound to UTC millisecond
precision. UTC/local selection changes presentation, not the requested instant.
Validation compares exact instants through the 30-day limit; a positive 1ns
interval is valid, but 30 days plus 1ns is not. This URL/API contract does not
imply nanosecond storage in ADX. Nonexistent local DST times are rejected;
ambiguous edited local times use the platform's earlier occurrence. Use an
explicit offset or UTC for an unambiguous repeated local time.

Run search determines historical membership, ordering, totals and pagination.
Local run membership includes metric-file registration timestamps as well as
run and event timestamps, before applying the result limit. A long-running run
can therefore remain in range after a JSONL import without a new lifecycle
event; its original run timestamps are not rewritten. Local experiment discovery
also includes child metric-file registration, even when later imports move the
experiment update time beyond an earlier selected interval. Registration time,
not the metric sample's wall time, defines this import activity. Both local
discovery and run search compare timestamp instants, including fractional seconds
and equivalent timezone offsets, before applying result limits. Custom and
relative Stellar bounds retain nanosecond precision through API serialization.
Unscoped Kusto experiment discovery enforces the configured maximum discovery
lookback on `since`, `window` and absolute interval lengths before querying;
the existing project-scoped exemption is unchanged.
Within the same workspace/source scope, matching snapshot runs (project and run
ID) retain their authoritative lifecycle, evidence timestamps and detail fields;
search-only fields and metric summaries remain available. Local search rows also
carry authoritative `outcome_state` / `liveness_state` resolved from their own
record and indexed metrics using the snapshot resolver. Thus runs absent from a
truncated snapshot retain correct liveness without additional queries. Existing
legacy classification and success-gate filtering remain unchanged. After selecting
and limiting historical Kusto members, search looks up their latest retained
terminal marker and ordinary metric, independent of the selected interval and
snapshot cap. It matches exact workspace/project/group/run identities in batches
of at most 200, returning at most two evidence rows per identity. File/in-memory
sources reuse their already loaded rows; projection and remote-write use the
existing query transport. These bounds limit transfer, not ADX scan cost.
Evidence-query failure fails the Kusto source; auto retains local results with
its existing warning and local-first duplicate policy. A successful empty lookup
is explicitly unknown, never authoritative historical running. Matching snapshot
authority replaces the page outcome/liveness/reason/source as one coherent group.
Missing snapshot classification fields fall back to search classification. Snapshot-only runs
never enter the range-filtered list. Lifecycle describes the latest available
evidence, not a reconstruction of run state at the historical range end.
Reconciliation uses a linear-time identity lookup over the existing responses;
it does not issue additional requests or recompute lifecycle rules in the browser.

The inventory reader selects the exact canonical `unbounded-cloud.io/site`
label first and the exact deprecated `net.unbounded-cloud.io/site` migration
label only when the canonical value is empty. It never performs fuzzy label
matching or substitutes region, zone, or pool for site identity. If no GPU node
has either supported label, the map remains grouped by ordinary region and pool
placement without presenting an Unbounded site visualization. Partial coverage
keeps every unlabeled GPU node in an explicit Unknown bucket and reports the
coverage gap. When both exact labels are non-empty and disagree, inventory
preserves the canonical value and source key while reporting the conflict.

Region, zone, and pool remain separate placement fields. Nodes are
RDMA-advertised only when status exposes a positive `rdma/*` capacity or
allocatable resource. The site label is a topology boundary, not CNI health,
and RDMA capability is not health. Node cards separately show Ready and
scheduling state, NVIDIA model, GPU capacity and allocation, utilization, and
continuous condition evidence.

GPU allocation requires cluster-wide Pod visibility. Counts include active,
scheduled, non-terminal Pods and Kubernetes init/restartable-init scheduling
semantics. Free capacity is reported only for Ready, non-cordoned nodes. Missing
or unauthorized Pod visibility, MIG, and DRA allocation cases fail closed to
Unknown instead of presenting zero assignments or free GPUs.

Current CPU and memory utilization similarly require cluster-wide access to
`metrics.k8s.io/v1beta1` Node metrics. Missing, unauthorized, or malformed
samples preserve inventory, surface an explicit error, and fall back only to an
exact cluster-plus-instance ADX match. Missing per-GPU telemetry does not create
empty load and temperature tiles; the node's Telemetry status remains Unknown.

Continuous GPU/NVLink and InfiniBand evidence comes from an explicit allowlist
of monitoring-owned Kubernetes Node condition families. A condition family is
**Observed OK** only when every required family appears exactly once with a
fresh `False` observation. A fresh `True` condition is **Fault** and takes
precedence over missing coverage in another family. Missing, duplicate,
malformed, stale, future-dated, or `Unknown` evidence stays **Unknown**. The
Portal uses a 15-minute freshness window and tolerates at most one minute of
future clock skew. Evaluation is keyed by condition type, so condition ordering
does not affect the result.

Per-GPU ADX telemetry reports whether every expected inventory GPU has a
complete row-remap verdict; it is not presented as comprehensive GPU health.
Each node links to the inline Fleet GPU detail table for the underlying metrics.

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
Custom cost ranges must start and end on whole UTC hours; unsupported partial
hours return **400** before querying Kusto. Allocation buckets use `[start, end)`:
a 10:00-11:00 range includes the 10:00 bucket, not the bucket starting at 11:00.
Raw utilization samples retain inclusive bounds, and relative lookback requests
retain their existing behavior. Partial-hour costs are not prorated or inferred.
Opening a new Cost Custom range defaults to the previous complete UTC hour.
Both display timezones remain available. Apply validates actual UTC instants
before navigation or a new request; nonaligned user input is never rounded.
Direct invalid URLs still reach backend validation and can be corrected using
the controls. Other boards retain their generic custom defaults.
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
   continuous GPU, NVLink, and InfiniBand Node conditions. Two richer signals
   remain unavailable:
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
