# Tau Kind smoke

> **Status:** `smoke/debug`
> **Intended use:** prove the local Tau -> Kueue -> Kubernetes Job and KubeRay paths on a disposable Kind cluster.
> **Not for:** GPU scheduling, AKS node labelling, CSI storage, or production queue policy.

This example is the smallest local end-to-end for `tau run`: it creates or
reuses a Kind cluster, installs Kueue and KubeRay through the repo's TauGrid
distribution chart, applies a test-only CPU Kueue lane, submits both `tau.yaml` (batch Job) and
`tau-ray.yaml` (KubeRay RayJob), waits for Kueue admission plus a ready KubeRay
RayCluster, and checks the lifecycle through the Kubernetes, Kueue, and Ray CRDs.
Completion mode also runs one actor on each of two Ray workers and requires the
RayJob and its Kueue Workload to finish successfully.

From `cli`:

```bash
make test-kind-e2e
```

The script keeps the Kind cluster by default so you can inspect it after the
run:

```bash
kubectl --context kind-tau-kind get job,pod,workloads.kueue.x-k8s.io,localqueues.kueue.x-k8s.io -n ray
```

Set `TAU_KIND_DELETE_CLUSTER=1` to delete the cluster after a successful run, or
`TAU_KIND_RECREATE=1` to force a clean cluster before running. The cluster name
defaults to `tau-kind`; override it with `TAU_KIND_CLUSTER_NAME`. Set
`TAU_KIND_RAY_WAIT_FOR_COMPLETION=1` when you also want to wait for the Ray driver to finish, verify the RayJob Kueue
Workload reaches `Finished`, and assert the Ray driver log contains the smoke
completion marker.

The two-worker Ray smoke requires an 8 GiB Docker or Podman VM. On macOS with
Podman:

```bash
podman machine stop
podman machine set --memory 8192
podman machine start
```

The script fails fast when the selected container engine is unhealthy or reports
less than 7680 MiB usable memory. Override the guest-usable thresholds with:

```bash
TAU_KIND_ENGINE_MIN_MEMORY_MIB=7680
TAU_KIND_ENGINE_RECOMMENDED_MEMORY_MIB=7800
TAU_KIND_GET_TIMEOUT_SECONDS=30
TAU_KIND_DELETE_TIMEOUT_SECONDS=180
TAU_KIND_CREATE_TIMEOUT_SECONDS=600
```

`tau-ray.yaml` sets local-only CPU/memory requests because Kind runs the Ray
head and both workers on one Kubernetes node. The head has enough memory for
the local Ray control plane and driver, while the workers stay small enough to
exercise both worker pods within the 8 GiB provider budget. Production configs
should use platform presets or cluster-sized resource requests instead.

`tau cluster install` exercises the same Helm-only distribution path used by a
fresh cluster. The private-image-dependent Tau controller and optional TauGrid
services are disabled for this local fixture; `kind-kueue-lanes.yaml` is applied
as test data after installation. Kind has no GPU or durable CSI storage, so the
workloads are ephemeral and CPU-only.
