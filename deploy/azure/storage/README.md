# TauGrid 0.1 durable storage contract

Storage is external platform desired state in TauGrid 0.1. TauGrid,
`tau-core-controller`, `TauWorkspace`, and the Tau CLI do not provision, adopt,
own, resize, migrate, or delete StorageClasses, PVCs, PVs, CSI drivers, or cloud
storage. A platform owner must create a compatible PVC in the workload namespace
and keep its lifecycle in reviewed Helm, GitOps, or IaC.

Every Tau workload that needs durable storage mounts two paths, by design:

| Path    | Backing                                  | Environment variable | Purpose                                  |
|---------|------------------------------------------|----------------------|------------------------------------------|
| `/data` | existing PVC named by `storage.data_pvc` | `TAU_DATA_DIR`       | durable datasets / checkpoints / results |
| `/mnt`  | `emptyDir` (node-local)                  | `TAU_HOT_DIR`        | fast, ephemeral per-run scratch          |

`/mnt` is provisioned automatically by the job template (node-local `emptyDir`).
`/data` is optional and only appears when a workload names an existing claim.
PVCs are namespaced: a claim must exist in the resolved workload namespace,
normally `TauWorkspace.spec.target.namespace`. Tau preflight rejects a missing
or unbound named claim; Tau does not create or repair it.

The conventional `blob-training` name remains supported and is still used by
some defaults and shared clusters. The name is historical: it means "the
durable `/data` claim," not "must use Azure Blob." Any explicitly configured
compatible existing PVC can satisfy the contract.

## Optional Azure operator examples

The manifests in this directory are optional starting points for platform
owners. They are not installed by TauGrid and neither backend is required:

| File | Backend | Consider when | Cost / caveats |
|------|---------|-----------|----------------|
| [`tau-data-premium-blob.yaml`](./tau-data-premium-blob.yaml) | Premium Blob (BlobFuse, `Premium_LRS`) | Object-backed RWX storage fits the workload. | Object store, not POSIX: no safe concurrent writes to the same file (rank-0 writes checkpoints, per-run subdirs). The example dynamically creates Azure resources through the CSI driver. |
| [`tau-data-lustre.yaml`](./tau-data-lustre.yaml) | Azure Managed Lustre (AMLFS) | Training I/O is the bottleneck: checkpoint-heavy full FT, huge shuffles, multi-node shared scratch. | Real parallel POSIX FS (no concurrent-write caveat), high aggregate bandwidth. **Region-bound** and bills on provisioned size 24/7. You must fill region/RG/size. |

Review namespace, capacity, reclaim policy, identity, locality, cost, and CSI
prerequisites before adopting either example. A platform may instead use Azure
Files, NFS, an existing static PV/PVC, or another Kubernetes-compatible backend.

On current shared research clusters the live PVC names may differ from the
generic `blob-training` default:

| PVC | Namespace | Intended use |
| --- | --- | --- |
| `<lustre-pvc>` | `ray` | Flex-only shared Lustre dataset/checkpoint mount for cluster-pinned training runs. |
| `<datasets-pvc>` | `ray` | Default examples and shared dataset/checkpoint `/data` mount. |
| `<datasets-pvc-alt>` | `ray` | Homebase dataset/checkpoint mount for workloads that need that storage account. |

These live names are **worker-local compatibility mounts**, not the common
MultiKueue any-worker contract. General worker parity uses the canonical
`ray/blob-training` name on every eligible worker, provisioned independently
per cluster with equivalent durable `/data` semantics and **no cross-region byte
replication**. Directed canaries may still omit `storage.data_pvc` (emptyDir
only) or stay cluster-pinned on one of the worker-local PVCs above.

For Ray workloads, Tau creates the configured output directory under `/data`
with an init container before the nonroot Ray image starts. This avoids the
common fresh-PVC failure where `/data` is mounted `root:root 0755` and the
trainer cannot create `/data/checkpoints/workflows/<run>`.

## Create storage through the platform delivery path

Storage is cluster policy, not a Tau CLI side effect. If one of the Azure
examples fits, vendor and adapt it in the cluster's reviewed Helm/GitOps/IaC
path:

```bash
# Optional Premium Blob example:
kubectl apply -f applications/taugrid/storage/tau-data-premium-blob.yaml

# OR optional Azure Managed Lustre example (edit REPLACE_* first):
kubectl apply -f applications/taugrid/storage/tau-data-lustre.yaml

# Verify the claim is Bound before submitting jobs:
kubectl -n ray get pvc blob-training
```

The AKS AI Runtime test clusters use the overlay-gated
`applications/tau-storage` ArgoCD app. Other platform owners create the chosen
claim in every workload namespace that needs it. The TauGrid distribution does
not choose a backend, copy these examples into a cluster, or delete storage on
uninstall.

## Example prerequisites

- The target workload namespace.
- The CSI driver required by the selected example:
  - Premium Blob → `blob.csi.azure.com` (AKS `azureBlobCsiDriver` addon).
  - Lustre → `azurelustre.csi.azure.com`.
  ```bash
  kubectl get csidrivers | grep -E 'blob.csi.azure.com|azurelustre.csi.azure.com'
  ```

## Beyond the defaults

- **Share an existing account/container** (datasets already live somewhere):
  keep the static PV/PVC binding in your cluster registration or GitOps repo,
  next to the storage account, container, identity, and secret choices it
  depends on. The committed default here stays the zero-config dynamic premium
  blob path.
- **Add Lustre as extra scratch alongside blob `/data`** (rather than replacing
  it): add a cluster-specific AMLFS PVC and mount it at `/lustre` via
  `storage.mounts`; keep region, subnet, and billing choices with the cluster
  registration.
- **Override the PVC name** per manifest with `storage.data_pvc`.

Storage lifecycle functionality is an explicit post-0.1 fast-follow. This
release does not promise a future API shape or delivery date.
