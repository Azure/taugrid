# Cluster configuration overlays

This directory contains Kubernetes configuration that is selected per cluster.
It is not installed by `tau cluster install` or the `charts/taugrid` Helm
chart. Platform owners should review, adapt, and apply only the overlays needed
by their chosen capabilities, normally through GitOps.

| Path | Purpose | Apply when |
| --- | --- | --- |
| `queues/` | Kueue ResourceFlavors, ClusterQueues, LocalQueues, and targeted quota patches | The baseline `jobqueue` does not express the cluster's GPU pools, team quotas, or serving policy |
| `azure/storage/` | Azure Blob and Azure Managed Lustre storage examples | Workloads need the corresponding persistent storage backend |
| `dashboards/` | Ray dashboard definitions | A compatible Grafana/observability stack is available |

These manifests are templates, not portable defaults. Before applying one,
verify its namespace ownership, node labels and taints, Kueue resource names and
quotas, CSI drivers, cloud permissions, and any external observability
dependencies. A Kueue nominal quota is admission policy; it does not create or
reserve matching physical CPU, memory, or GPU capacity.

The Helm distribution owns the reusable TauGrid control plane. Keep
cluster-specific policy here rather than extending the distribution chart with
environment-specific values.
