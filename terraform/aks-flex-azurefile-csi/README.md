# Azure File CSI Driver on AKS Flex nodes

This Terraform root installs a pinned, customer-managed upstream Azure File CSI
Driver for Linux AKS Flex or other externally registered nodes. It gives
TauGrid customers a repeatable installation and validation path without
depending on AKS-managed CSI addon DaemonSets to recognize external nodes.

It does not create AKS, Flex nodes, a storage account, storage credentials, or
TauGrid. Customers retain ownership of the storage backend, credentials,
capacity, networking, data lifecycle, driver upgrades, and incident response.

## Why this is a separate driver

Normal AKS agent pools can use the AKS-managed `file.csi.azure.com` addon. A
second Helm release using that same CSI identity would conflict with the
managed addon. This root therefore installs the upstream driver as
`file.flex.csi.azure.com`, gives its Kubernetes resources distinct names and
ports, disables Windows components, and targets its node DaemonSet only to the
labels in `flex_node_selector`.

PVCs for Flex nodes must use a StorageClass whose `provisioner` is
`file.flex.csi.azure.com`. Existing AKS StorageClasses using
`file.csi.azure.com` continue to use the managed addon and are not fixed by
this release.

## Prerequisites

- Terraform 1.9 or later
- Helm and kubectl access to the existing AKS cluster
- A verified kubeconfig context
- Linux Flex nodes with a stable label that distinguishes them from normal AKS
  agent pools
- Network connectivity from every selected node to the Azure Files endpoint
- An existing Azure Storage account and file-share authentication plan

The module pins the official upstream `azurefile-csi-driver` Helm chart and
uses its MCR-hosted images.

## Prepare Azure Files credentials

The optional StorageClass uses a pre-created Kubernetes Secret so the storage
account key never enters Terraform state. Create the Secret in each workspace
namespace that will provision or mount shares:

```bash
kubectl --context <aks-context> -n <workspace-namespace> create secret generic \
  azure-storage-account \
  --from-literal=azurestorageaccountname='<storage-account-name>' \
  --from-literal=azurestorageaccountkey='<storage-account-key>'
```

Treat the command as an example delivery mechanism only. Prefer the customer's
approved secret-management workflow and avoid recording the key in shell
history. The Secret name must match the Terraform input. The StorageClass uses
the external provisioner's `${pvc.namespace}` token to read that Secret from
the namespace of each claim.

## Plan and apply

```bash
cd terraform/aks-flex-azurefile-csi
cp terraform.tfvars.example terraform.tfvars
# Set the exact kube context, Flex node label, and storage inputs.

terraform init
terraform plan -out=azurefile-flex.tfplan
terraform show -no-color azurefile-flex.tfplan
terraform apply azurefile-flex.tfplan
```

Store shared-environment state in an encrypted remote backend with locking.
The state owns only the Helm release and, when enabled, the StorageClass; it
does not contain the storage account key.

## Validate every eligible node

Confirm the distinct driver exists and its DaemonSet covers every Flex node:

```bash
kubectl --context <aks-context> get csidriver file.flex.csi.azure.com
kubectl --context <aks-context> get daemonset csi-azurefile-flex-node \
  -n kube-system -o wide

for node in $(kubectl --context <aks-context> get nodes \
  -l 'kubernetes.azure.com/cluster=flex-h200' -o name); do
  kubectl --context <aks-context> get csinode "${node#node/}" \
    -o jsonpath='{range .spec.drivers[*]}{.name}{"\n"}{end}' |
    grep -Fx file.flex.csi.azure.com
done
```

Then create a PVC using `storageClassName: azurefile-flex` and run a pod pinned
through the intended TauGrid workload profile. The StorageClass uses
`Immediate` binding because TauGrid validates that platform-managed PVCs are
already Bound before workload submission. After the claim binds, the canary
must mount it, write a file, read it back, and delete it using the same
ServiceAccount and security settings as the customer workload. A `Bound` PVC
alone is not enough.

## Safe customization

- Keep `driver_name` distinct from `file.csi.azure.com` while the AKS-managed
  addon is enabled.
- Keep `flex_node_selector` narrow. Do not use AKS-only label spoofing to make
  the managed addon schedule on external nodes.
- Add the actual Flex taints to `linux_tolerations`; the default tolerates all
  taints because CSI registration must cover every selected workload node.
- The module disables the upstream Azure File proxy, AZNFS installation, and
  managed-identity helper defaults. The checked-in Secret-backed SMB path does
  not need them, and enabling them can mutate node-global host services shared
  with another Azure File driver.
- Review upstream release notes before changing `azurefile_csi_version`.
- For identities, NFS, private endpoints, or dynamic storage-account creation,
  extend the module only after reviewing the upstream driver's Azure
  permissions and network requirements. The checked-in StorageClass uses an
  existing account and a pre-created SMB credential Secret.

## Removal

Stop or move workloads using `azurefile-flex`, then remove their PVC/PV
resources according to the chosen reclaim policy before running
`terraform destroy`. Destroying the driver while volumes are mounted disrupts
workloads. The default `Retain` policy avoids deleting the backing Azure file
share with the StorageClass lifecycle.
