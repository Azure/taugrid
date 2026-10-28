# TauGrid Kueue

This chart installs the maintained AKS Kueue package as a separate
self-managed Helm release for TauGrid.

The chart must be installed in `kueue-system`. The packaged Kueue controller
uses an extension controller service account that is allowlisted only in that
namespace. It cannot be embedded in the TauGrid umbrella release in
`tau-system`.

```bash
helm upgrade --install taugrid-kueue \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid-kueue \
  --version 0.4.7 \
  --namespace kueue-system \
  --create-namespace \
  --wait
```

The chart enables MultiKueue controller capability but does not create worker
credentials, routing objects, queues, resource flavors, or workload profiles.
TauGrid owns its baseline queue and policy resources through the separate
`taugrid` release.

On AKS 1.34 or later, prefer the managed `microsoft.kueue` extension when the
platform wants Azure to own controller installation and upgrades. Do not
install this chart and the managed extension on the same cluster.
