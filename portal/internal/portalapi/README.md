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
  Independent source panels expose loading, refresh, last-success time, and
  explicitly stale retained data when a refresh fails.
- **Boards** — each `internal/portal/{cluster,cost,jobs,ray,nodes,runs}` package
  exposes `Board(ctx, source, Options) (Snapshot, error)`. Two data-source
  families back them: Kubernetes (Jobs/Ray/Nodes/Runs share one client-go
  `kubeclient` reader) and Kusto (Cluster/Cost share a `kustoquery` shell-out
  querier).
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
| Platform | Fleet › Health | `/portal/fleet?view=health` | `cluster.Board` (per-GPU health, Kusto) | ① (IB/NPD/AlertRule = ③) |
| Platform | Fleet › Utilization | `/portal/fleet?view=util` | reuses `/api/portal/cluster`, re-sorted by util%, + node CPU/mem (`nodeutil.Board` → `/api/portal/nodeutil`, Kusto) | ① (heatmap/per-team = ③) |
| Platform | Fleet › Compute | `/portal/fleet?view=compute` | `nodes.Board` (hardware inventory, K8s) | ① |
| Platform | Kueue | `/portal/jobs` | `jobs.Board` (Kueue queue snapshot) | ① (PriorityClass = ③) |
| Platform | Ray | `/portal/ray` | `ray.Board` (dashboard Services, K8s) | ① |
| Platform | Observability | `/portal/observability` | none — placeholder | ③ |
| Platform | Cost | `/portal/cost` | `cost.Board` (namespace GPU-hours, Kusto) | ① (per-user/team = ③) |
| Experiments | Native workspace | `/portal/experiments` | canonical `/api/v2/stellar` reads | ① |

Workloads and Platform retain their `/portal` overview landings; Experiments
goes directly to `/portal/experiments`, without a separate overview page.
The overview API remains available for existing board consumers.
The three Fleet boards share one page via in-page sub-tabs (Health |
Utilization | Compute); the legacy `/portal/{cluster,gpu,nodes}` paths still
resolve to the matching Fleet sub-tab so existing deep-links keep working.

## Data interpretation and recovery

`GET /api/portal/overview?view=workloads` returns profiles, queue counters, and
admitted-workload links without querying optional fleet, GPU, cost, or Ray
sources. The unqualified overview API retains its complete response.
Admission is quota reservation, not proof that pods are running.

Job details expose `diagnostics` for workloads, pods, events, and tracking:
`ready`, `empty`, `unavailable`, or `not_configured`. Indexed metrics can enable
a scoped Stellar link while a job is active; a terminal lifecycle marker is not
required. Retried source failures do not silently erase previously displayed
evidence or label it fresh. Explicit client/access rejections (including 401,
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

2. **Fleet Health depth — InfiniBand / NPD / AlertRule.** Today's Fleet Health is
   per-GPU DCGM health. The proposal's richer signals are not portal-readable:
   - **InfiniBand port/flap** state lives in a node-local file written by
     `check_ib_flaps.sh`; it must first be surfaced as a node condition (via the
     collector) or pushed to ADX before a board can read it.
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
