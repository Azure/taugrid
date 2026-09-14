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

The smoke then upgrades a legacy workspace CRD through `tau cluster install`,
checks that the existing `tau-researcher-v1` workspace keeps its UID and role,
and server-validates the new `researcher` alias. It also submits a generated
RayService to real Kueue/KubeRay controllers and checks admission priority,
both worker Pods' readiness probes on port 9000, memory-backed `/dev/shm`,
and an HTTP response from the Serve service.

The RayService fixture calls the distributed renderer, then replaces GPU
requests with small CPU resources and removes worker cross-host anti-affinity
so it fits the single-node Kind cluster. The CPU head keeps its normal 4Gi
memory limit for the dashboard and Serve controller; each Ray object store
is capped at 128Mi to fit the fixture's 256Mi shared-memory mount.
Its app places two CPU replicas on
separate Ray workers. This tests operator integration, not GPU inference or
multi-host scheduling; unit tests cover the unmodified GPU placement contract.

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

`tau cluster install` exercises both the fresh Helm installation and the
existing-release Tau CRD update path. The Tau controller image is built locally;
optional TauGrid core services are disabled for this fixture.
`kind-kueue-lanes.yaml` is applied as test data after installation. Kind has no
GPU or durable CSI storage, so the workloads are ephemeral and CPU-only.
