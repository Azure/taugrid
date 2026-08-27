# TauGrid AKS Terraform

This Terraform root creates a GPU-enabled AKS environment and then invokes the
repository's supported `tau cluster install` workflow. TauGrid owns Kueue,
KubeRay, the Tau controller, GPU monitoring, the baseline queue, and Portal.

The default GPU pool is one `Standard_NC24ads_A100_v4` node. It is billable and
requires matching regional quota. Change `gpu_vm_size`, `gpu_count_per_node`,
`gpu_monitoring_sku_name`, `gpu_class`, and `gpu_series` together for another
supported GPU SKU. The GPU ResourceFlavor labels match the node-pool labels.

## Prerequisites

- Terraform 1.9 or later
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
state is `Registered`, then refresh the AKS resource provider:

```bash
az feature register --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview
az feature show --namespace Microsoft.ContainerService --name ManagedGPUExperiencePreview --query properties.state --output tsv
az provider register --namespace Microsoft.ContainerService
az provider show --namespace Microsoft.ContainerService --query registrationState --output tsv
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

# The tracked quickstart enables the complete ADX-backed observability path.
enable_adx                = true
enable_lifecycle_recorder = true
workspace_namespace       = "taugrid-default"
adx_cluster_name          = ""

```

The variable defaults remain opt-in so an invocation without a reviewed
variable file cannot create billable ADX resources or remote telemetry. The
tracked `terraform.tfvars.example` deliberately enables ADX and lifecycle
recording for the complete observability path. Function definitions use Kusto
`skipvalidation` so a first install can create them while adx-mon creates their
dependent tables asynchronously. This avoids treating a transient schema-order
race as a failed chart installation.
Set
both feature flags to `false`
for a Kubernetes-only Portal deployment.

An empty `adx_cluster_name` generates a stable globally-scoped candidate with
the form `taugrid<8-hex-characters>`. The suffix is derived from the
subscription, resource group, and AKS cluster names, so repeated plans do not
change it and the value is known before apply. Override it only if Azure reports
that candidate is already in use. The resolved value is available through
`terraform output -raw adx_cluster_name`.

Managed Entra authentication and local administrator accounts are enabled by default. Terraform uses a local administrator kubeconfig for installation commands. Set `aks_admin_group_object_ids` only when Entra groups also need cluster-admin access. Keep `azure_rbac_enabled = false` so TauWorkspace RoleBindings enforce workspace access.

Review and apply one saved plan:

```bash
terraform plan -out taugrid.tfplan
terraform show -no-color taugrid.tfplan
terraform apply taugrid.tfplan
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

When lifecycle recording is enabled, Terraform also bootstraps the empty
`workspace_namespace` before installing TauGrid. This satisfies the lifecycle
recorder's namespace prerequisite without claiming the TauWorkspace contract.
When `bootstrap_workspace` is configured, Terraform applies the TauWorkspace
after installing TauGrid; otherwise, a later `tau workspace create` command
adopts that namespace. Either path reconciles the workspace labels, Pod
Security Admission labels, output scope, LocalQueue, RBAC, and optional
workload ServiceAccount. Use the exact same namespace in both places. Reserved
namespaces such as `default`, `tau-system`, `tau-platform`, and `kube-*` are
rejected.

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
Create the workspace as the local administrator against the namespace that
Terraform bootstrapped, wait for the complete Workspace contract, then run the
standard smoke test:

```bash
tau workspace create taugrid-default \
  --namespace taugrid-default \
  --system-namespace tau-system \
  --principal-name <entra-group-object-id> \
  --apply
kubectl -n tau-system wait \
  --for=jsonpath='{.status.phase}'=Ready \
  workspace/taugrid-default \
  --timeout=5m
tau workspace check taugrid-default
```

Portal is installed as `tau-portal` in the `tau-system` namespace with a ClusterIP Service. Terraform does not expose it outside the cluster. For an operator diagnostic session:

```bash
kubectl -n tau-system port-forward service/tau-portal 18080:80
```

An authenticated HTTPS proxy is required before giving researchers a browser
URL. With the tracked ADX settings, Portal uses its query identity for the
Kusto-backed boards, the lifecycle recorder writes through its separate
`Metrics.Ingestor` identity, and adx-mon manages the
`Metrics.TauExpRunLifecycle` schema. Verify that path after Workspace readiness:

```bash
kubectl -n tau-system rollout status \
  deployment/tau-lifecycle-recorder \
  --timeout=25m
kubectl -n adx-mon describe managementcommand taugrid-lifecycle-schema
kubectl -n tau-system logs deployment/tau-lifecycle-recorder --tail=100
```

Terraform creates a least-privilege ADX ingestion identity, enables Portal run
history, and asks adx-mon to create the `Metrics.TauExpRunLifecycle` schema.
For a one-step installation, Terraform first creates the target workload
namespace if needed so the TauGrid chart can install the lifecycle recorder.
It does not create a TauWorkspace or add workload policy to that namespace
unless `bootstrap_workspace` is configured.

## Optional workspace bootstrap

Set `bootstrap_workspace` to create TauGrid's single v0 Entra-backed
workspace as part of the same apply. The Entra group object ID is used both as
the external principal reference and the Kubernetes Group subject. The
TauWorkspace controller creates or reconciles the workload Namespace, its
`jobqueue` LocalQueue, and namespace-scoped researcher RBAC.

```hcl
workspace_namespace = "taugrid-default"

bootstrap_workspace = {
  name                  = "taugrid-default"
  entra_group_object_id = "00000000-0000-0000-0000-000000000000"
}
```

Terraform applies the native TauWorkspace CR after `tau cluster install` and
waits for it to become Ready. It also enables Portal's restricted operator
Jobs scope for only this workspace namespace and `jobqueue`. The Portal
Service remains ClusterIP-only; this does not create a researcher browser
endpoint or replace the required authenticated HTTPS proxy.

Leave `bootstrap_workspace` unset for a platform-only installation. A platform
owner can then create the workspace later with `tau workspace create` or a
reviewed TauWorkspace manifest. Removing `bootstrap_workspace` from a retained
cluster stops Terraform from applying the CR but intentionally does not delete
an existing workspace or its workloads; remove it through the workspace
administration workflow after reviewing the impact.

## Destroy

Destroying the resource group removes the cluster and stops GPU billing:

```bash
terraform destroy
```
