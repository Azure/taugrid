# NVIDIA GPU Operator on existing AKS Flex nodes

This Terraform root installs one pinned NVIDIA GPU Operator release into an
existing AKS cluster that already has attached Flex GPU nodes. It does not
create AKS, attach Flex nodes, install TauGrid, or manage host NVIDIA drivers.

GPU Operator is cluster-scoped. Install it once per AKS cluster, not once per
Flex node group. By default, its operands target every worker labeled
`feature.node.kubernetes.io/pci-10de.present=true`, not only Flex nodes. A
dedicated AKS cluster for Flex nodes is the safest model. In a mixed cluster,
review every GPU software stack before applying: two device plugins, driver
managers, container toolkits, or DCGM exporters must not own the same node.

The checked-in defaults use the working TauGrid Flex ownership model with
conservative MIG behavior:

- GPU Operator chart `v26.3.0`
- Node-image-provided host driver (`driver.enabled=false`)
- Explicitly selected container-toolkit ownership and device plugin enabled
- DCGM exporter enabled on port `9400`
- MIG unmanaged (`mig.strategy=none`, MIG Manager off)
- GPU taint tolerations for `sku=gpu:NoSchedule` and `nvidia.com/gpu`

## Prerequisites

- Terraform 1.9 or later
- Helm and kubectl access to the existing AKS cluster
- A kubeconfig context that cannot be confused with another cluster
- Flex GPU nodes labeled `kubernetes.azure.com/cluster=flex-<name>`
- A working host NVIDIA driver on every target Flex node

For a mixed cluster, exclude every GPU node that another stack owns before
applying:

```bash
kubectl --context <aks-context> label node <non-flex-gpu-node> \
  nvidia.com/gpu.deploy.operands=false
```

This is an operational policy, not a one-time inventory step: ensure newly
autoscaled non-Flex GPU nodes receive the same label before GPU Operator can
schedule operands. If that cannot be guaranteed, do not use this example on a
mixed cluster. Flex and non-Flex nodes under different GPU-stack owners must
also use disjoint VM SKUs/TauGrid GPU profiles. TauGrid selects per-profile
DCGM routing by instance type, so it cannot safely route two ownership models
for the same SKU; use a dedicated cluster if ownership cannot be separated.

Fetch credentials with Azure CLI and verify the context before Terraform uses
it:

```bash
az aks get-credentials \
  --resource-group <aks-resource-group> \
  --name <aks-cluster-name> \
  --overwrite-existing

kubectl config get-contexts
kubectl --context <aks-context> cluster-info
kubectl --context <aks-context> get nodes \
  -l 'kubernetes.azure.com/cluster' \
  -L kubernetes.azure.com/cluster,node.kubernetes.io/instance-type
```

If the kubeconfig uses Microsoft Entra authentication, install `kubelogin` and
authenticate with the identity Terraform should use. Do not replace Entra
credentials with embedded administrator credentials for automation.

## Verify driver and toolkit ownership

The Flex node-image owner owns the host driver in this model, so this example
deliberately disables the GPU Operator driver DaemonSet. Before setting
`host_drivers_preinstalled=true`, run a temporary driver check on every Flex
GPU node using your platform-approved privileged diagnostic process. The check
must execute the host's `nvidia-smi`, not infer driver health from Kubernetes
allocatable resources. For example, where node debugging is approved:

```bash
for node in $(kubectl --context <aks-context> get nodes \
  -l 'kubernetes.azure.com/cluster=flex-<name>' -o name); do
  kubectl --context <aks-context> debug "$node" -it \
    --image=ubuntu:22.04 -- chroot /host \
    nvidia-smi --query-gpu=name,driver_version --format=csv,noheader
done
```

If the nodes do not already have a functional driver, stop. Do not turn
`driver.enabled` on without a Flex image/kernel compatibility review.

Also determine whether the node image already owns NVIDIA Container Toolkit and
the `nvidia` containerd runtime. Keep `toolkit_enabled=true` only when GPU
Operator should configure that runtime. Set it false for a preinstalled,
platform-managed toolkit. In either case, set `toolkit_ownership_reviewed=true`
only after documenting the owner.

## Plan and apply

```bash
cd terraform/aks-flex-gpu-operator
cp terraform.tfvars.example terraform.tfvars
# Set kube_context and acknowledge the verified host driver.

terraform init
terraform plan -out=gpu-operator.tfplan
terraform show -no-color gpu-operator.tfplan
terraform apply gpu-operator.tfplan
```

This state owns only the Helm release. Store shared-environment state in an
encrypted remote backend with locking.

## Validate the Flex GPU stack

Wait for the ClusterPolicy and confirm the Operator operands run on the intended
GPU nodes:

```bash
kubectl --context <aks-context> wait \
  --for=jsonpath='{.status.state}'=ready \
  clusterpolicy/cluster-policy \
  --timeout=20m

kubectl --context <aks-context> get pods -n gpu-operator -o wide
kubectl --context <aks-context> get nodes \
  -l 'kubernetes.azure.com/cluster=flex-<name>' \
  -L nvidia.com/gpu.deploy.device-plugin,nvidia.com/gpu.deploy.dcgm-exporter
kubectl --context <aks-context> get nodes \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}'
```

The DCGM endpoint for TauGrid is:

```text
http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
```

For a dedicated Operator-only GPU cluster, configure the TauGrid umbrella chart
to consume that existing exporter globally:

```yaml
gpu-monitoring:
  dcgmHealth:
    source: exporter
    exporterUrl: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
```

For a mixed cluster, keep the global source for the other GPU stack and override
only each GPU Operator-backed Flex profile:

```yaml
gpu-monitoring:
  gpuSkus:
    h200:
      dcgmHealth:
        source: exporter
        exporterUrl: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
```

Replace `h200` with the TauGrid GPU profile used by the Flex nodes. Do not point
managed GPU profiles at this Service: `internalTrafficPolicy: Local` correctly
has no endpoint on nodes excluded from GPU Operator.

When adx-mon is enabled for Fleet and Cost telemetry, add a node-local static
scrape target for the Operator exporter. Scope `hostRegex` to the Flex node
names so collectors on excluded GPU nodes do not scrape the Service:

```yaml
collector:
  prometheusScrape:
    extraStaticTargets:
      - hostRegex: '^flex-.*'
        url: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
        namespace: gpu-operator
        pod: nvidia-dcgm-exporter
        container: nvidia-dcgm-exporter
```

Then install or upgrade TauGrid through `tau cluster install`; do not enable the
`terraform/aks` root's `self_managed` device-plugin/DCGM path on the same
nodes.

## MIG behavior

The defaults use `mig.strategy=none` and leave MIG Manager disabled. This does
not disable MIG or remove existing partitions; it leaves host MIG state
unchanged while the device plugin ignores MIG resources. If whole GPUs are
required, first verify `nvidia-smi -q` reports MIG disabled on every target
node. Normalize any existing MIG state through a separately reviewed,
drained, reboot-aware operation. Then validate `nvidia.com/gpu` resources and
raw DCGM metrics before relying on Portal utilization.

## Removal

`terraform destroy` removes GPU Operator and its managed operands. It does not
detach or delete Flex nodes. Workloads requiring `nvidia.com/gpu` will stop
scheduling after removal, so drain or stop them first.
