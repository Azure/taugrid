# TauGrid with AKS-managed operators

> **Status:** `operator runbook`
> **Intended use:** install TauGrid on AKS while the `microsoft.kueue` and
> `microsoft.kuberay` extensions own operator APIs, controllers, webhooks,
> upgrades, and removal.
> **Not for:** migrating a running self-managed installation in place.

This is the preferred AKS installation path. The Azure extensions own Kueue
and KubeRay. The TauGrid Helm release owns Tau APIs, `tau-core-controller`,
workspace intent, and optionally the baseline queue policy. It never adopts,
upgrades, or removes extension-owned resources.

## Provision the extensions

The `tau` CLI validates managed extension ownership and compatibility but does
not create Azure resources. Provisioning remains an explicit Azure operation:

```bash
export TAU_EXTENSIONS_RG="<resource-group>"
export TAU_EXTENSIONS_CLUSTER="<cluster-name>"

./examples/aks-managed-extensions/install.sh
```

The script:

1. Requires AKS 1.34 or later.
2. Refuses an existing non-extension Kueue or KubeRay controller.
3. Creates both extensions with automatic minor-version upgrades.
4. Waits for Azure provisioning and the Kubernetes controllers.
5. Runs `tau cluster install --mode managed`.

The CLI accepts the managed compatibility lines Kueue 0.19.x and KubeRay
1.7.x. The service chooses exact extension versions and may auto-upgrade them.
If Azure moves an extension outside those lines, upgrade TauGrid to a release
that declares compatibility before continuing.

## Platform-composed queue policy

Managed operators do not imply TauGrid-owned queue policy. When GitOps owns
the baseline `ClusterQueue`, flavors, topology references, or priority policy,
disable TauGrid's baseline policy:

```bash
tau cluster install \
  --mode managed \
  --context "$TAU_CONTEXT" \
  --set baselineQueue.enabled=false
```

`tau-core-controller` still owns `taugrid-gpu-topology` and TauWorkspace queue
intent unless its first-party component is explicitly disabled outside this
supported installation workflow.

## Migration boundary

Do not switch an existing release between `managed` and `helm`. Managed
extensions enforce singleton operators, so TauGrid cannot verify a replacement
while the active self-managed controller is still reconciling. The CLI rejects
the mode change before any Helm mutation.

Same-cluster ownership migration is not supported because it cannot meet the
replacement-before-removal safety requirement. For a planned migration:

1. Export all Kueue and Ray custom resources and record active workloads.
2. Provision a separate cluster in the target mode.
3. Verify its operators, compatibility, policy, and workload conformance.
4. Migrate reviewed workload state and traffic.
5. Drain and uninstall the old cluster through its current owner only after
   the replacement is serving.

Never edit Helm ownership annotations or use `--force-conflicts` to transfer
ownership.

## Removal

Run `tau cluster uninstall --yes` first. In managed mode this removes only
affirmatively TauGrid-owned resources. It does not remove either extension or
platform-owned policy.

Removing the managed Kueue extension can delete Kueue custom resources.
Export required state and follow the current Microsoft Learn removal guidance.
Managed KubeRay removal leaves Ray resources without a reconciler.

Managed Kueue feature availability is service-versioned. In particular,
MultiKueue availability may differ from the pinned Helm path; verify the
installed extension's supported features before creating MultiKueue policy.

