# TauGrid with managed Kueue and KubeRay extensions

> **Status:** `operator runbook`
> **Intended use:** install TauGrid on an existing AKS 1.34 or later cluster
> while Azure cluster extensions own the Kueue and KubeRay controllers.
> **Not for:** production use while the managed extensions are in preview,
> clusters below Kubernetes 1.34, or clusters that already run self-managed
> Kueue or KubeRay controllers.

TauGrid's self-managed installation publishes a `taugrid-kueue` wrapper around
the maintained AKS Kueue chart and embeds the maintained AKS KubeRay chart in
the `taugrid` release. This recipe selects AKS-managed ownership instead.

This recipe uses the alternative managed-extension path:

1. Install `microsoft.kueue` and `microsoft.kuberay`.
2. Verify the extension resources, controller deployments, and CRDs.
3. Install TauGrid with `--kueue-mode=external` and
   `components.kuberayOperator.enabled=false`.
4. Let the TauGrid chart create its queue policy and first-party controllers
   against the extension-owned APIs.

Do not install this path beside the default TauGrid controller dependencies.
Managed Kueue rejects an existing self-managed Kueue controller. A rejected
KubeRay extension installation can still change CRD schemas and ownership.

## Prerequisites

- An existing managed-identity AKS cluster running Kubernetes 1.34 or later.
- Azure CLI, `kubectl`, `helm`, and `tau` on `PATH`.
- Permission to register resource providers, manage cluster extensions, fetch
  AKS credentials, and create cluster-scoped Kubernetes resources.
- No self-managed Kueue or KubeRay controller in the cluster.

The script uses the preview release train and enables automatic minor-version
upgrades. Extension versions are selected by the service for the target
cluster. Query the installed versions after installation rather than assuming
they match the self-managed chart pins.

## Install

```bash
export TAU_EXTENSIONS_RG="<resource-group>"
export TAU_EXTENSIONS_CLUSTER="<cluster-name>"

./examples/aks-managed-extensions/install.sh
```

Optional inputs:

```bash
export KUBECONFIG="/tmp/<cluster-name>-extensions.kubeconfig"
export TAU_EXTENSIONS_CHART="<local-chart-path-or-OCI-reference>"
```

`TAU_EXTENSIONS_CHART` is useful when validating a local chart checkout. When
unset, `tau cluster install` uses the chart version pinned into the Tau CLI.

## Verify

The script waits for both extension resources and controller deployments, then
checks the required Kueue and Ray CRDs. It finishes with:

```bash
tau cluster validate installation --context "<kube-context>"
```

Tau's installation validator reports Kueue and KubeRay as skipped because the
TauGrid Helm release does not own those deployments. The recipe's extension
checks are the readiness source for those two components.

The managed Kueue extension has its own supported feature and integration
boundary. This recipe does not claim the self-managed `taugrid-kueue` chart's MultiKueue
configuration. Use the self-managed TauGrid chart for the pinned MultiKueue
contract documented by TauGrid.

## Removal

Uninstall TauGrid before removing either extension:

```bash
tau cluster uninstall --context "<kube-context>"
```

Removing managed Kueue deletes Kueue custom resources, including queues and
resource flavors. Export any configuration that must survive before deleting
the extension. Removing managed KubeRay leaves Ray CRDs and workloads in place,
but no operator remains to reconcile them. Follow the current Microsoft Learn
removal guidance for each extension rather than treating extension deletion as
a reversible Helm uninstall.

## References

- [Managed Kueue extension](https://learn.microsoft.com/azure/aks/kueue-overview?pivots=extension)
- [Managed KubeRay extension](https://learn.microsoft.com/azure/aks/kuberay-overview)
- [AKS cluster extensions](https://learn.microsoft.com/azure/aks/deploy-extensions-az-cli)
