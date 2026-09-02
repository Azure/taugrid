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
- `tau`, `helm`, and `kubectl` on PATH. Install the matching released `tau`
  binary for Linux or macOS with `install.sh`. On Windows amd64, download the
  release `install.ps1`, run it, and add `%LOCALAPPDATA%\TauGrid\bin` to PATH;
  alternatively pass the installed executable path to the verification script.
- PowerShell 7. Terraform uses it for local installation commands and the
  maintainer verification entry point on Windows, Linux, and macOS. Linux,
  WSL, and macOS deployments can instead set
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

## Maintainer deployment verification

For a maintainer-only, disposable, ADX-backed Portal deployment verification on
any supported host OS:

```powershell
pwsh -File terraform/aks/Invoke-TauGridAksVerification.ps1
```

This integration verification creates billable Azure resources and requires a
subscription with sufficient regional quota. It is not part of the normal
offline or CI test suite.

The script resolves `tau` for the active OS and, before it accesses Azure,
requires `tau version --short` to be exactly `v<TauGridVersion>` (by default
`v0.4.0`). It rejects development builds and stale CLIs because the CLI and
chart readiness contracts must match. Pass `-TauCommand` with an absolute path
to a matching released binary when it is not on PATH; paths containing spaces
are supported.
It accepts the ADX SKU and capacity, plus the GPU VM SKU and its matching
monitoring, class, and series metadata. The script queries the Azure Resource
Manager ADX SKU catalog as an early validation; Azure service-side creation
remains authoritative. If the AzureRM provider loses state during a long ADX
create, the script waits for the one cluster in its isolated resource group,
imports it, and resumes the apply. It writes tfvars, state, plan, and
Terraform data beneath `terraform/aks/generated/` and never reads a local
`terraform.tfvars` file. Its state is isolated with Terraform's local
state-file flags and `init -backend=false`; it does not select a backend for
normal Terraform deployments. The verifier also gives Terraform a per-run
generated directory, so its kubeconfig, Helm state, and generated values do
not conflict with another local Terraform process.

By default the script validates the platform and that Portal reads GPU
telemetry from ADX. To also create an Entra-backed workspace, run a smoke job,
and validate Portal lifecycle history from ADX, explicitly enable that path and
provide a group object ID from the active Entra tenant:

```powershell
pwsh -File terraform/aks/Invoke-TauGridAksVerification.ps1 `
  -EnableBootstrapWorkspace `
  -BootstrapWorkspaceEntraGroupObjectId "<entra-group-object-id>"
```

## Standard deployment

```bash
cd terraform/aks
terraform init
```

Keep the deployment inputs in a local `terraform.tfvars` file so every later
`plan`, `apply`, and `destroy` uses the same environment. Do not commit this
file. Start from the tracked template. In Windows PowerShell, use `Copy-Item`;
on Linux, WSL, and macOS, use `cp`. PowerShell 7 is the default Terraform
interpreter; uncomment the Bash setting in the template only to use Bash
instead:

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

An empty `adx_cluster_name` generates a stable globally-scoped 20-character
candidate with the form `taugrid<13-hex-characters>`. The 13-character (52-bit)
suffix is derived from the subscription, resource group, and AKS cluster names,
so repeated plans do not change it and the value is known before apply. Override
it only if Azure reports that candidate is already in use. Terraform preserves the
deployed name on later applies, including a legacy 15-character name generated by
an earlier version of this module. The resolved deployed value is available with
`terraform output -raw adx_cluster_name`.

An explicit `adx_cluster_name` is used unchanged when creating a new cluster. An
existing Azure Data Explorer cluster cannot be renamed, so changing this variable
later does not replace the cluster automatically. Treat an intentional rename as
an explicit migration that preserves the required data before replacement.

### Verify a legacy generated-name upgrade

If the current state was created when the automatic name used
`taugrid<8-hex-characters>`, create a plan before applying this upgrade. The
following check must print `true`; `false` means the plan would replace the ADX
cluster and must not be applied:

```bash
terraform plan -out=adx-name-upgrade.tfplan
terraform show -json adx-name-upgrade.tfplan | jq -e '
  [
    .resource_changes[]?
    | select(.address == "azurerm_kusto_cluster.this[0]")
    | select((.change.actions | index("delete")) and (.change.actions | index("create")))
  ]
  | length == 0
'
```

Saved plan files can contain sensitive values. Keep `adx-name-upgrade.tfplan`
local and remove it through your approved cleanup process after inspection.

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
Terraform bootstrapped, then wait for the complete Workspace contract:

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

Run the [A100 GPU quickstart](../../examples/aks-gpu-quickstart/README.md) after
the workspace is ready to verify CUDA execution. The maintainer verification
script above creates a temporary workspace and RayJob when invoked with
`-EnableBootstrapWorkspace`.

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
