# Portal Ray + Stellar dual-link demo

> **Status:** `operator runbook`
> **Intended use:** submit a single `tau run --config` job whose portal detail
> page lights **both** links at once — "Ray dashboard" and "Open in
> Experiments" (Stellar).
> **Not for:** first-time onboarding or clusters without a Ready TauWorkspace
> and a writable `/data` PVC — this example depends on live platform state.

A bounded, dependency-free CPU `RayJob` that exercises both portal detail-page
links:

- **"Ray dashboard"**: the job runs as a RayJob, so KubeRay creates a
  RayCluster with a head Service — exactly what the portal keys the Ray
  dashboard button off. The link is reachable while the head is Ready and
  during the renderer's short post-completion grace period; it is not durable
  run history.
- **"Open in Experiments" (Stellar)**: the trainer publishes immutable
  `metrics-history-attempt-0/*.jsonl` chunks; the Tau metrics-offload sidecar watches them,
  remote-writes rows to adx-mon/Kusto, and — critically — publishes a terminal
  `tau/run_status` marker on shutdown. The portal only lights the Stellar link
  once that marker lands in Kusto.

## Prerequisites

1. **A pinned metrics-offload image.** `metrics.offload.enabled: true` requires
   `TAU_METRICS_OFFLOAD_IMAGE` (or `--metrics-offload-image`) set to a
   **taugrid-portal** image pinned by digest. The sidecar runs
   `taugrid-portal experiment offload metrics`, a verb only the taugrid-portal
   image installs (at `/usr/local/bin/taugrid-portal`); the plain `tau` image
   does **not** contain it and the container would fail to exec. It must be
   pinned by `@sha256:` — a `:latest` tag is rejected.
2. **A namespace with a Ready TauWorkspace and a writable `/data` PVC.** The
   offload sidecar remote-writes to adx-mon and writes a small SQLite buffer;
   the trainer publishes immutable JSONL chunks under `/data`. The canonical
   `tau-default` namespace satisfies both on `<cluster>` and
   `<cluster>`: it is the target of the cluster-wide `default`
   TauWorkspace and holds the GitOps-owned `blob-training` PVC.

## Run

Client dry-run first — this also confirms the offload sidecar renders (its
transient SQLite/spool buffers resolve under the `/var/run/tau` emptyDir, not
`/data`):

```bash
# from the repository root
make install-tau-cli
export TAU_METRICS_OFFLOAD_IMAGE=<platform-supplied-taugrid-portal@sha256:digest>
tau run --config cli/examples/portal-ray-stellar/tau.yaml --dry-run=client
```

The rendered RayJob must carry
`tau.azure.com/stellar-experiment-id: ray-plus-stellar` and
`tau.azure.com/stellar-group-value: default`, mount `blob-training`, and render
the `metrics-offload` sidecar with the immutable history glob. It must not carry
the retired `tau.azure.com/stellar-experiment-title` annotation.

Submit:

```bash
tau run --config cli/examples/portal-ray-stellar/tau.yaml
tau run status portal-ray-stellar --watch
kubectl -n tau-default get rayjob portal-ray-stellar
kubectl -n tau-default get pod -l ray.io/cluster -o wide
kubectl logs -n tau-default -l tau.azure.com/run-id=portal-ray-stellar -f
```

Then open the portal detail page for the run and confirm both links are lit:

```
/portal/runs/tau-default/<run-name>
```

The deployed portal is scoped to `tau-default` (`portal.workloadNamespace` in
each cluster's `applications/taugrid-portal` overlay), so the run appears on the
Runs list board as well as at the detail URL above.

`compute.workers` counts dedicated execution pods; Tau adds a separate
CPU-only system head. For the multi-node acceptance pass, set it to `2` or greater and use a distinct
workload name/output directory. The driver reads `TAU_NUM_WORKERS`, uses a
strict-spread placement group, and only worker 0 publishes metrics. Confirm pod
node names separately when distinct physical hosts are required.
