# portalapi — unified observability portal

`portalapi` serves the runtime's single read-only web surface: the `/portal`
frontend shell and the `/api/portal/*` board APIs. In single-workspace
mode it also reuses the mounted Stellar experience (`/stellar*`,
`/api/*stellar*`) unchanged for Experiments.
It is started by `taugrid-portal portal serve` and, in-cluster, by a Deployment behind an
internal load balancer. It follows the persona-centered UI direction proposed in
[PR #827](https://github.com/Azure/taugrid/pull/827).

## Shape

- **Shell** — `assets/index.html` is a hand-written SPA (no build step). It paints
  a persona sidebar (ML Engineer / Platform) and dispatches by `location.pathname`
  to per-board render functions, each of which fetches one `/api/portal/*` board.
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
- **Experiments embed** — single-workspace mode keeps the persona sidebar visible and
  loads the mounted Stellar SPA in a same-origin `<iframe src="/stellar">`.
  Managed workspaces may use a local `/stellar` path or an explicit HTTPS
  experiment endpoint, but `experimentsUrl` requires a Kusto-backed source.
  Portal permits only workspace-aware read routes (experiment/run discovery,
  snapshots, series, capabilities, page assets) and blocks mutation, artifact,
  label, saved-workspace, and perf-harness routes until those contracts carry
  workspace-qualified ownership. Stellar's handler hard-codes `X-Frame-Options:
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

## Tab UI roadmap

The shell groups today's boards under three top-level tabs — **Workloads**,
**Platform**, **Experiments** — evolving the persona direction from PR #827.
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
| Experiments | Stellar | `/portal/experiments` | mounted Stellar SPA, embedded in a same-origin iframe | ① |

`Overview` (`/portal`) is retained as each tab's landing: Workloads and Platform
render their own headline cards; Experiments links into the embedded Stellar
board. The three Fleet boards share one page via in-page sub-tabs (Health |
Utilization | Compute); the legacy `/portal/{cluster,gpu,nodes}` paths still
resolve to the matching Fleet sub-tab so existing deep-links keep working.

## Deferred backends (③)

Each of these is a page or field the proposal draws but the portal has **no data
source for today**. They are listed here — not implemented — so the gap between
"the mock renders it" and "the runtime emits it" stays explicit.

1. **Cost per-user/team + budgets.** `cost.Board` aggregates GPU-hours by
   *namespace* only, from `GpuHealth()` utilization samples. Per-user/team
   attribution and budget burn need a CostTracking store (e.g. `GpuCostDaily` /
   `NamespaceCostMonthly` as adx-mon SummaryRules), a second querier, and a
   user/team dimension that does not exist in the current schema. No such Go code
   exists yet.

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
