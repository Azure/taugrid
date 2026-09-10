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
- Approved Azure credentials usable by both the AzureRM and AzAPI providers
- `tau`, `helm`, and `kubectl` on PATH. Install the matching released `tau`
  binary for Linux or macOS with `install.sh`. On Windows amd64, download the
  release `install.ps1`, run it, and add `%LOCALAPPDATA%\TauGrid\bin` to PATH;
  alternatively pass the installed executable path to the verification script.
- PowerShell 7.3 or later (including its native argument-passing support).
  Terraform uses it for local installation commands and the
  maintainer verification entry point on Windows, Linux, and macOS. Linux,
  WSL, and macOS deployments can instead set
  `command_interpreter = ["bash", "-c"]`. The Function waiter supports Bash 3.2
  (including stock macOS Bash) and later.
- For ADX installation, `jq` 1.6 or later and
  [Mike Farah `yq` v4](https://github.com/mikefarah/yq) on PATH in either
  interpreter. A different `yq` implementation is not compatible.

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
- Existing AKS clusters with attached Flex GPU nodes use the separate
  [GPU Operator Terraform example](../aks-flex-gpu-operator/README.md). Do not
  combine that cluster-scoped Operator with this root's `self_managed` stack on
  the same nodes.

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
`v0.4.2`). It rejects development builds and stale CLIs because the CLI and
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

### Function readiness and bounded installation recovery

The Terraform Function waiter renders the local adx-mon chart with the same
ordered base and environment values, release name, and release namespace used
by its installation attempts. Every rendered Function is required; disabled
items are not. The initial controller-only install uses `functions.enabled=false`;
the subsequent waiter upgrades use `--reset-values` with the supplied values,
not that bootstrap override or values inferred from live Helm release storage.
Chart contents and the shared readiness policy participate in Terraform's
installation replacement triggers.

Readiness requires every expected namespace/name to exist with current-generation
`Success`. The Helm managed-by and instance labels and release-name/release-namespace
annotations must match. Missing or conflicting ownership fails before an upgrade
and is checked again while polling; the waiter never adopts an object. Unrelated
Functions cannot satisfy a missing requirement, block readiness, or become retry
targets. Missing/stale status waits with diagnostics; unreadable, malformed,
duplicate, or incomplete sources fail closed.

Only current-generation permanent failures identifying ADX throttling are eligible
for compensation. Invalid KQL and other permanent failures stop immediately.
The six-attempt default retry limit is retained. Deletes send Kubernetes
`DeleteOptions` with **both UID and resourceVersion preconditions** for each
verified expected, release-owned object. A conflict or disappearance causes
re-observation within the same budget, including another ownership preflight
before Helm runs; other delete errors stop. There is no name-only delete fallback.

Terraform uses release `adx-mon` in release namespace `adx-mon`. For an explicitly
authorized standalone installation, the scripts also accept `-ReleaseName` and
`-ReleaseNamespace` (PowerShell), or optional eighth and ninth positional arguments
(Bash). The resource namespace comes from each rendered Function, including
`global.namespace`, and can differ from the Helm release namespace. Supply the
actual ordered values files; do not run these installers merely to inspect a
shared namespace. An empty rendered Function set is rejected unless intentionally
acknowledged with `-AllowNoFunctions` or `ADX_ALLOW_NO_FUNCTIONS=true`; an empty or
failed chart render is never accepted, even with that opt-out.

This is **installation mitigation**, not an upstream adx-mon controller fix:
direct `helm install`/`upgrade` does not run it, and throttling recovery without
deletion remains tracked by [#162](https://github.com/Azure/taugrid/issues/162).
Function `Success` with `skipvalidation` does not prove underlying table/schema
readiness or lossless startup history ([#190](https://github.com/Azure/taugrid/issues/190)).
Live candidate, fault-recovery, and schema qualification require separate approval.

Offline regressions (no Azure or Kubernetes access) run through the existing
entry points and require Helm, jq, yq, and Python 3 in addition to the interpreter:

```bash
bash terraform/aks/test-wait-for-adx-functions-ready.sh
pwsh -NoProfile -File terraform/aks/test-wait-for-adx-functions-ready.ps1
```

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

The lifecycle recorder ADX `Ingestor` assignment uses AzAPI with a custom retry
rule for HTTP errors containing `AAD principal was not found` (case-insensitive),
which can occur while a new managed identity propagates from Entra to ADX.
It preserves `principalType = App` and the identity's **client ID**, not its
object/principal ID. The locked AzAPI 2.12.0 provider uses exponential backoff
with jitter, a 10-second base interval, and a 180-second maximum delay. Azure SDK
retries for 408, 429, 500, 502, 503, and 504 also still apply; a nonmatching
permanent HTTP error is not retried by the custom rule.

The create operation has a 60-minute budget. On deadline/cancellation AzAPI can
spend up to another five minutes reading the resource to retain its ID in state.
The HTTP retry rule does **not** resubmit creation when a long-running operation
returns HTTP 200 with a terminal `Failed` status, even if its error mentions the
principal. That failure is surfaced, not silently treated as success. A matching
HTTP error from a poll retries that poll, not the original create. Consequently,
successful configuration or mock-plan tests are not proof of fresh-install
recovery: qualification must identify which response path Azure actually uses.
An assignment reaching `Succeeded` is also not full-install acceptance: verify
recorder readiness and durable Portal history separately. As tracked in #190,
Helm readiness and Kusto `skipvalidation` do not establish that the lifecycle
schema exists before the first workload.

On failure, retain the Terraform diagnostic (including the last retryable HTTP
error when available), elapsed time, assignment ARM ID, and the identity's
client and tenant IDs from
`terraform state show 'azurerm_user_assigned_identity.lifecycle_recorder[0]'`.
Use the Azure Activity Log for the identity creation timestamp; Terraform does
not expose it on this resource. Check the approved tenant/subscription and ADX
diagnostics without changing the identity or granting access outside Terraform.
After resolving the cause, create and review a new plan with the same inputs
before retrying. If Azure created a grant that is absent from state, stop for an
explicit, approved import recovery instead of deleting or recreating it.

### Upgrade an existing lifecycle recorder grant

Keep lifecycle recording enabled, use the same backend/workspace and input
files as the existing deployment, and retain the checked-in provider lock file.
Run `terraform init` without `-upgrade`. AzAPI 2.12.0 implements Terraform's
cross-provider state-move protocol, supported by this module's Terraform 1.9
minimum. The checked-in `moved` block transfers
`azurerm_kusto_database_principal_assignment.lifecycle_recorder[0]` to
`azapi_resource.lifecycle_recorder_principal_assignment[0]` while preserving its
ARM ID. Do **not** run `state rm`, hand-edit state, create a second grant, or
substitute an object ID. Back up state using the deployment's approved process.
Before planning, record the existing grant's `id`, `principal_id` (client ID),
and `tenant_id` from
`terraform state show 'azurerm_kusto_database_principal_assignment.lifecycle_recorder[0]'`.
The plan's `prior_state` is already moved/refreshed and is not an independent
copy of the original AzureRM values.

AzAPI first refreshes moved state using its latest indexed Kusto API version
(`2025-02-14` in 2.12.0), then compares it with the configured `2024-04-13` API
body. This needs authorized ARM read access and must be qualified against the
actual deployment. A syntactically valid `moved` block alone is not sufficient
evidence. If the body matches, AzAPI can retain the moved API version in state.

Create a **normal refreshed plan**, not a targeted or `-refresh=false` plan,
and inspect it before any apply:

```bash
terraform plan -out=lifecycle-upgrade.tfplan
pwsh -NoProfile -File ./Test-LifecycleRecorderUpgradePlan.ps1 \
  -PlanPath ./lifecycle-upgrade.tfplan \
  -ExpectedAssignmentId '<assignment ARM ID recorded before planning>' \
  -ExpectedClientId '<client ID recorded before planning>' \
  -ExpectedTenantId '<tenant ID recorded before planning>'
terraform show -no-color lifecycle-upgrade.tfplan
```

In PowerShell, put the checker command on one line instead of using the Bash
line continuation. The checker requires exactly the expected move and a
`no-op` action or a state-only `update`, with the same ARM ID, name, parent,
client ID, tenant, `App` type, and `Ingestor` role. Only `retry` and `timeouts`
may differ: the locked AzAPI 2.12.0 provider marks these as state-only updates
and skips its external request when no other attributes change. This records
the new retry policy without recreating or modifying the Azure grant. Unknown
values, any other attribute differences (including body, API type, and export
settings), create/delete/replacement, missing move, or incomplete plans stop
the upgrade for investigation. A checker pass is a guard, not a substitute for
qualification of an actual refreshed existing-state plan.
Review **all** other changes too; this checker only guards the grant.
Only apply that saved plan through the normal approved deployment process.
Saved plans and state can contain secrets; keep them local and clean them up
through that process.

An absent old address makes the move a no-op on fresh installations and on
already-migrated deployments. The upgrade checker is intentionally for the
**first** AzureRM-to-AzAPI migration, not these cases. If both feature flags
were already disabled, no identity or grant is created. Disabling an existing
recorder still removes its resources as before; do not combine that intentional
decommissioning with this grant-retention upgrade. If both assignment addresses
already exist in state, stop and reconcile ownership with an authorized
operator rather than overriding either state entry.

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
