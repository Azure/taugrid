# taugrid-portal

`taugrid-portal` is the Tau experiment and observability web surface. It was
split out of the `tau` CLI so that submitting a workload no longer links an
embedded web frontend, a SQLite-backed experiment store, and a Kusto query
stack into the same binary a researcher runs from a laptop.

The verb surface is unchanged. Everything that was `taugrid-portal experiment ...` or
`taugrid-portal portal ...` is now `taugrid-portal experiment ...` and
`taugrid-portal portal ...`, with the same flags, output, and behaviour. The
hidden `exp` alias is preserved.

## Commands

| Command | Responsibility |
| --- | --- |
| `taugrid-portal experiment` | Track, import, query, compare, and visualize experiment state through expstore and Stellar. |
| `taugrid-portal portal` | Serve the read-only cross-system observability portal. |
| `taugrid-portal version` | Print build and version information. |

## Layout

| Path | Contents |
| --- | --- |
| `frontend` | React/TypeScript portal, client-side routing, and workspace-scoped server-state queries. |
| `internal/expstore` | The local-first experiment store: run packets, metrics, artifacts, search index. |
| `internal/expcockpit` | Stellar: snapshot construction and the embedded HTML/CSS/JS frontend. |
| `internal/expapi` | The HTTP server behind `experiment serve` and the mounted Stellar experience. |
| `internal/expimport` | TensorBoard, Weights & Biases, and JSONL metric importers. |
| `internal/expcapture` | Maps a `core/status` run profile into experiment store records. |
| `internal/autocapture` | Reconciles Kubernetes Job and RayJob state into experiment runs. |
| `internal/portalapi`, `internal/portal/*` | The portal HTTP surface and its per-board data sources. |
| `internal/artifactoffload`, `internal/blobstore`, `internal/jsonlutil` | Durable artifact upload and the JSONL plumbing that feeds it. |

Shared contracts — run status, queues, topology, run config, experiment
identity, workload labels — live in
[`../core`](../core) and are imported by both this binary and the tau
CLI.

## Stable boundaries

- Local expstore remains authoritative for run packets, artifacts, checkpoints,
  and recovery state. ADX/Kusto is a downstream analytics projection only, not
  the source of truth, and telemetry is not the only copy of non-scalar run
  state.
- The portal and Stellar are read-only. They never mutate cluster state.
- Secret values belong in Kubernetes Secret or Key Vault references, never in
  checked-in configs, ConfigMaps, annotations, logs, metrics, or screenshots.

## Build

Frontend development and Make-based builds require Node.js 22.20 or newer and npm in
addition to Go. Dependencies are locked in `frontend/package-lock.json`.

```bash
cd portal
make build              # builds the frontend and ./bin/taugrid-portal
make test               # Go tests
make lint               # frontend type checking and Go linting
make frontend-dev       # Vite development server
make frontend-build     # regenerate the embedded production assets
```

For development, run `taugrid-portal portal serve` with your usual workspace
and data-source flags on `127.0.0.1:8080`, then open the URL printed by
`make frontend-dev` under `/portal/`. Vite proxies `/api` and `/stellar` to that
backend; it does not bypass workspace authentication or supply mock data.

`portal serve` handles SIGTERM and SIGINT by closing its listener and allowing
active HTTP requests up to five seconds to drain. If draining times out, it
closes remaining HTTP connections and reports a nonzero exit. This signal
handling is scoped to `portal serve`; metrics offload keeps its separate
termination handler so its final metrics/status flush is not cancelled.

React source lives in `frontend/`; generated, content-hashed assets live in
`internal/portalapi/assets/` and are embedded in the Go binary. Include regenerated
assets with frontend changes. This keeps direct `go build ./...` and
`go test ./...` usable without Node; those commands use the checked-in frontend
and do not rebuild it. CI rebuilds the frontend and rejects stale assets.

The container image is built from [`../images/taugrid-portal`](../images/taugrid-portal).
Its Node build stage regenerates the frontend before Go compilation; the final
distroless image still runs only the Go binary.

## Frontend boundaries

React owns the portal shell and boards under `/portal/`. React Router owns
navigation; TanStack Query owns remote data and workspace-scoped caches.
Transient controls remain component state rather than a second global store.
Workspace selection is URL state, not authorization: the Go backend and trusted
identity proxy remain responsible for access checks. Unavailable or remote
workspaces must not silently fall back to local data.

The **Experiments** tab opens the native React workspace at `/portal/experiments`
directly, without an intermediate Overview page or a second navigation sidebar.
The UI uses the Portal's Experiments naming; existing `/stellar` links and API
paths remain compatible. The workspace is native React,
not an iframe or a wrapper around the legacy renderer. Discovery, run selection,
metric charts, comparison, and research evidence share the portal's navigation,
workspace context, and query cache. Experiment, metric, and step-range selections
remain deep-linkable. Report artifacts are excluded from the native evidence
views, counts, and links; other media and config evidence remain available.
When a snapshot omits selected runs, the evidence panels disclose incomplete
coverage and link to exact-run views rather than claim those runs have no evidence.

The **Experiment summary** disclosure includes run-scoped launch details using
the same visible-run selection as charts. Expanding it lazily shares the full
snapshot query with Research evidence. Requested GPU totals, GPUs per worker,
worker/pod counts, GPU class, image, entrypoint, profile, workspace and recorded
queue are not presented as observed GPU allocations. Missing values remain
"Not recorded", including explicit CPU requests of zero versus unknown counts.
MIG requests are labeled as slices (total and per worker), with the resolved
resource mode/name and MIG profile separate from GPU model/class. These optional
fields come from renderer inputs, never resource-profile name guesses. Counts
without a recorded unit are labeled "GPU units (unit unspecified)"; legacy GPU
context whose basis is unknown is labeled separately. No slice count is treated
as a physical GPU count.

New `tau run` submissions attach the credential-free `tau.azure.com/launch`
annotation (`core/experiment.Launch` v1). Run-profile capture persists it in the
existing run-scoped `tau.launch` tag; metrics offload carries the same tag to
local/Kusto records. The tag uses `base64url:`-encoded JSON to preserve the legacy
comma-separated tag transport; snapshots also expose an optional `launch`
object. Old stores/backends remain readable. No runconfig or Python SDK schema
change is required. This capture allowlists resolved resource metadata and omits
environment, arbitrary config values, workload arguments and credential URLs.
The stored Tau command is a normalized CLI launch reference, **not** exact
original shell argv or the workload command; unsafe legacy commands are withheld.
Existing saved configs supply available fields when launch metadata is absent.

The native React UI and agents consume the same canonical `/api/v2/stellar/*`
API on the same TauGrid Portal service. The standard backend runs in-process:
no second Stellar service or `experimentsBackend.url` is required. For the
cluster's Kusto-backed deployment, both UI and API need configured ADX query
access and identity permissions, plus recorded experiments; serving the Portal
does not itself provision ADX or create experiment data. Local expstore remains
an option for local development.

An explicitly trusted, separately deployed backend is an optional advanced
override behind the portal's workspace authorization gate. The browser stays
same-origin in either case; it does not make cross-origin authenticated requests.

An external `experimentsUrl` by itself is only legacy navigation metadata, not
permission to proxy that server or fall back to the local store. Configure an
explicit trusted backend connection to use that source in the native workspace.

### Inherit a cluster-level ADX query connection

For the TauGrid umbrella installation, put this nonsecret connection in the
cluster's `taugrid-values.yaml` and pass it with `tau cluster install --values
taugrid-values.yaml` (or Helm's `--values` on install/upgrade):

```yaml
global:
  adx:
    queryConnection:
      endpoint: https://my-cluster.eastus2.kusto.windows.net
      database: Metrics
      clientID: 11111111-2222-3333-4444-555555555555
```

Helm records it once in the release. The default Portal inherits the query
endpoint/database and configures its ServiceAccount annotation and Workload
Identity pod label automatically. The native React UI and JSON examples below
then use that same in-process ADX backend, with fixed workspace
`taugrid-default`; no separate Stellar service or `experimentsBackend` is needed.
Existing experiment data/tables and network reachability are still required.

The platform must **first** provision a separate ADX Viewer-only identity,
enable cluster Azure Workload Identity, and federate that identity to the
cluster OIDC issuer, audience `api://AzureADTokenExchange`, and subject
`system:serviceaccount:tau-system:tau-portal` (adjust for overridden namespace/SA).
Do not reuse adx-mon's ingestion/admin identity. Installation does not create
Azure resources, federation, permissions, or data and cannot verify ADX roles.
Cost queries remain on the separately configured `CostTracking` database by
default and require Viewer permission there if used.

Nonempty `taugrid-core.portal.kusto.endpoint`/`.database` and an explicit
`taugrid-core.portal.serviceAccount.annotations.azure.workload.identity/client-id`
override inherited fields. Set the whole shared triple or leave it empty;
partial connections or empty explicit client-ID annotations fail rendering.
An absent connection preserves degraded Kusto behavior; local/auto mode does
not inherit it. The Service remains ClusterIP-only, and workspace-directory
routing is not enabled. See the [chart contract](../charts/taugrid/README.md#cluster-level-adx-query-connection)
for existing-ServiceAccount handling and override details.

### Direct JSON access for agents

For an authorized operator accessing a single-workspace deployment, the default
Service is `tau-portal`, ClusterIP-only, on port 80 in the install namespace
(`tau-system` by default). Keep this running in one terminal:

```bash
kubectl -n tau-system port-forward svc/tau-portal 8080:80
```

Open `http://127.0.0.1:8080/portal/experiments` for the UI, or use GET requests
from another terminal without browser cookies:

```bash
BASE=http://127.0.0.1:8080
curl --fail --silent --show-error "$BASE/api/portal/workspaces"

# Use an authorized workspace ID from discovery and your recorded project.
WORKSPACE=taugrid-default
PROJECT=my-project
curl --fail --silent --show-error --get "$BASE/api/v2/stellar/capabilities" \
  --data-urlencode "workspace=$WORKSPACE"
curl --fail --silent --show-error --get "$BASE/api/v2/stellar/experiments" \
  --data-urlencode "workspace=$WORKSPACE" --data-urlencode "project=$PROJECT" \
  --data-urlencode "limit=200"

# Set the experiment ID returned above, then select a run_id from runs.
EXPERIMENT=my-experiment
curl --fail --silent --show-error --get "$BASE/api/v2/stellar/runs" \
  --data-urlencode "workspace=$WORKSPACE" --data-urlencode "project=$PROJECT" \
  --data-urlencode "target=$EXPERIMENT" --data-urlencode "limit=200"
RUN=my-run-id
curl --fail --silent --show-error --get "$BASE/api/v2/stellar/snapshot" \
  --data-urlencode "workspace=$WORKSPACE" --data-urlencode "project=$PROJECT" \
  --data-urlencode "target=$RUN" --data-urlencode "mode=full" \
  --data-urlencode "include_static=true"

# Choose a recorded metric name from the snapshot's metric_options.
METRIC=train/loss
curl --fail --silent --show-error --get "$BASE/api/v2/stellar/series" \
  --data-urlencode "workspace=$WORKSPACE" --data-urlencode "target=$RUN" \
  --data-urlencode "run_id=$RUN" --data-urlencode "metric=$METRIC" \
  --data-urlencode "max_points=2000"
```

Inspect the full snapshot's `runs` entry matching `run_id` for recorded `configs`
and optional `launch`; there are no separate `/config` or `/metrics` routes.
Snapshots can include comparison runs, so do not assume every entry is the target
or treat a target missing from a capped snapshot as proof of absent evidence.
Discovery/search results are bounded: check `truncated`, and inspect snapshot
and series `warnings` and chart sampling metadata before claiming complete
coverage. Search `limit` defaults to 200 and is capped at 1000. Series
`max_points` defaults to 8000 (range 1–12000); use `start_step` and `end_step`
to narrow the window. Server run/metric-row caps still apply.

Use GET for these routes. Portal also allows target-bound `/artifacts` and
`/artifact` reads; broader backend capability advertisements do not grant access
to routes outside Portal's read allowlist or enable writes.

Cookie-free does **not** mean built-in Bearer/JWT authentication. Single-workspace
Portal has no application-level authentication: protect it with network controls
and Kubernetes RBAC, including access to port-forward. Managed workspace access
requires a trusted gateway that validates identity and supplies the configured
identity headers; agents must authenticate through that gateway, not self-assert
headers or bypass it with a direct connection. Use its authorized URL instead of
the port-forward URL for managed access.

### Separately deployed Stellar backends (optional advanced override)

Add the following to the authorized workspace record in the metadata-only
workspace directory:

```json
"experimentsBackend": {
  "url": "https://stellar.example/research",
  "bearerTokenFile": "/var/run/secrets/stellar/token"
}
```

The URL is the backend origin and optional base path, **not** its `/stellar`
page. The example forwards API requests beneath
`https://stellar.example/research/api/v2/stellar/`. Managed experiment sources
must be Kusto-backed, and the destination must enforce the same fixed workspace
ID as the directory record.

The optional token file is an upstream service credential mounted from a Secret,
not an agent access token or a token value in the workspace JSON.
Configure the destination's authentication
gateway to validate that credential when service authentication is required.
The portal does not forward browser cookies, viewer identity headers, bearer
tokens, or forwarded headers. Backend authority and token-file paths are not
included in browser discovery responses.

Use HTTPS for remote connections. Explicit HTTP connections are accepted only
for loopback or Kubernetes `.svc` / `.svc.cluster.local` service names; restrict
those services to trusted callers with network policy. Requests remain
read-only, workspace/source parameters are pinned server-side, upstream
redirects are rejected, and requests have a 15-second deadline. Unavailable
backends surface an error rather than using local data.

Native media requires an artifact attached to the requested, authorized target.
The native UI does not expose reports. Existing document API routes remain
compatible, but multi-file report bundles are not served through the managed
portal; document-only viewing does not load their relative assets.
