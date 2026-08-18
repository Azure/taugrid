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
under `generated/`. Its final provisioner installs the NVIDIA device plugin and
runs `tau cluster install`. Do not run a separate Helm installation for Kueue,
KubeRay, or gpu-monitoring.

After apply, use an operator kubeconfig and verify the environment:

```bash
eval "$(terraform output -raw get_credentials_command)"
tau cluster validate installation --timeout 10m
kubectl get nodes -l accelerator=nvidia
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\\.com/gpu}{"\\n"}{end}'
```

Create a workspace with a real Entra group object ID, then run the standard
smoke test:

```bash
tau workspace create taugrid-default --principal-name <entra-group-object-id> --apply
tau run smoke
```

Portal is installed as `tau-portal` in the `tau` namespace with a ClusterIP
Service. This is intentionally not an internet-facing endpoint. For an
operator diagnostic session:

```bash
kubectl -n tau port-forward service/tau-portal 18080:80
```

An authenticated HTTPS proxy is required before giving researchers a browser
URL. The default Portal deployment exposes Kubernetes-backed boards. Its
Kusto-backed boards require a separate ADX and workload identity configuration.

## Enable lifecycle history

Lifecycle history is a second apply. The target namespace is created by the
TauWorkspace, which itself requires the TauGrid installation from the first
apply. First create a workspace with its Entra group object ID:

```bash
tau workspace create taugrid-default --principal-name <entra-group-object-id> --apply
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
