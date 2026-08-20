# TauGrid AKS Terraform

This Terraform root creates a GPU-enabled AKS environment and then invokes the
repository's supported `tau cluster install` workflow. TauGrid owns Kueue,
KubeRay, the Tau controller, GPU monitoring, the baseline queue, and Portal.

The default GPU pool is one `Standard_NC24ads_A100_v4` node. It is billable and
requires matching regional quota. Change `gpu_vm_size`, `gpu_count_per_node`,
`gpu_monitoring_sku_name`, `gpu_class`, and `gpu_series` together for another
supported GPU SKU. The GPU ResourceFlavor labels match the node-pool labels.

## Prerequisites

- Terraform 1.6 or later
- Azure credentials usable by the AzureRM provider
- `tau`, `helm`, and `kubectl` on PATH
- PowerShell 7 on Windows. Linux and macOS users can set
  `command_interpreter = ["bash", "-c"]`.

## GPU stack modes

`gpu_stack_mode` selects how the GPU device plugin and DCGM exporter are
provided:

- `self_managed` is the default. Terraform installs the NVIDIA device plugin
  and the upstream NVIDIA DCGM exporter chart. This is the portable choice for
  AKS NVIDIA GPU node pools.
- `aks_managed_preview` uses the AKS Managed GPU Experience preview. AKS
  provides the driver, device plugin, and node-local DCGM exporter at port
  `19400`. This mode requires the subscription feature registration below and
  does not support GPU cluster autoscaling during the preview.

Before using `aks_managed_preview`, register the feature and wait until its
state is `Registered`:

```bash
az feature register --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview
az feature show --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview --query properties.state --output tsv
```

The AzureRM provider does not currently expose
`gpuProfile.nvidia.managementMode` for a standard AKS node pool. The preview
mode therefore adds `EnableManagedGPUExperience=true` when creating the pool.
When AzureRM exposes the managed GPU profile, replace that conditional tag in
`main.tf` with the provider field and remove this workaround. Keep the feature
registration until AKS makes the feature generally available.

## Deploy

```bash
cd terraform/aks
terraform init
```

Keep the deployment inputs in a local `terraform.tfvars` file so every later
`plan`, `apply`, and `destroy` uses the same environment. Do not commit this
file. Start from the tracked template:

```bash
cp terraform.tfvars.example terraform.tfvars
```

```hcl
subscription_id     = "<subscription-id>"
resource_group_name = "<resource-group-name>"
cluster_name        = "<cluster-name>"

# Optional ADX, adx-mon, and Portal Kusto integration.
enable_adx          = true
adx_cluster_name    = "<globally-unique-adx-cluster-name>"

```

By default, Terraform uses AKS local administrator credentials for its own
installation commands and the generated operator command. To opt in to managed
Entra AKS authentication, set `aks_admin_group_object_ids` to the administrator
group object IDs. Keep `azure_rbac_enabled = false` when TauWorkspace
Kubernetes RoleBindings should enforce workspace group access.

```bash
terraform apply
```

Terraform writes an ignored local admin kubeconfig and generated chart values
under `generated/`. In `self_managed` mode, it installs the NVIDIA device
plugin before any GPU readiness check. In `aks_managed_preview` mode, AKS owns
the driver, device plugin, and DCGM exporter host service. For the default A100
pool, Terraform then normalizes MIG mode, restarts the GPU VM scale set, and
waits for allocatable GPUs before running `tau cluster install`.
`normalize_gpu_mig = true` cannot be combined with GPU autoscaling because
later autoscaled nodes are not normalized. Set it false only for a GPU SKU that
does not require MIG normalization. Do not run a separate Helm installation
for Kueue, KubeRay, or GPU monitoring.

In `self_managed` mode, Terraform installs the upstream NVIDIA DCGM exporter
for TauGrid GPU monitoring, which uses the exporter Service with node-local
traffic. When ADX is enabled, adx-mon discovers that exporter by Pod annotation.
In `aks_managed_preview` mode, adx-mon collects metrics from the AKS GPU node
host service at port `19400`.

After apply, use an operator kubeconfig and verify the environment:

```bash
terraform output -raw get_credentials_command
```

Run the command printed above to fetch the operator kubeconfig. Then verify the
environment:

```bash
tau cluster validate installation --timeout 10m
kubectl get nodes -l accelerator=nvidia
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\\.com/gpu}{"\\n"}{end}'
```

The default path is an operator sandbox, not an Entra researcher handoff.
Create the workspace as the local administrator, then run the standard smoke
test:

```bash
tau workspace create taugrid-default --apply
tau run smoke
```

Portal is installed as `tau-portal` in the `tau` namespace with a ClusterIP
Service. Terraform does not expose it outside the cluster. For an operator
diagnostic session:

```bash
kubectl -n tau port-forward service/tau-portal 18080:80
```

An authenticated HTTPS proxy is required before giving researchers a browser
URL. The default Portal deployment exposes Kubernetes-backed boards. Its
Kusto-backed boards require a separate ADX and workload identity configuration.

## Enable lifecycle history

Lifecycle history is a second apply. The target namespace is created by the
TauWorkspace, which itself requires the TauGrid installation from the first
apply. First create a workspace:

```bash
tau workspace create taugrid-default --apply
```

Then add the following to the same local `terraform.tfvars` file and apply
again:

```hcl
enable_lifecycle_recorder           = true
workspace_namespace                 = "taugrid-default"
```

Terraform creates a least-privilege ADX ingestion identity, enables Portal run
history, and asks adx-mon to create the `Metrics.TauExpRunLifecycle` schema.

## Destroy

Destroying the resource group removes the cluster and stops GPU billing:

```bash
terraform destroy
```
