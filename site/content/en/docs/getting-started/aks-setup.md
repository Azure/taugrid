---
title: AKS setup
linkTitle: AKS setup
weight: 2
description: Validate subscription access, quota, cost controls, and choose an AKS provisioning path
---

{{< maturity status="ga" reviewed="2026-08-18" >}}

An Azure subscription is the first shared surface for an AKS-backed Tau environment. Tau does not create or govern the subscription. A platform owner prepares it with approved Azure tooling, provisions or selects an AKS cluster, and then hands a reachable provider-ready cluster to the TauGrid administrator.

This page covers **AKS setup**, the Azure/provider side of the boundary. It owns the subscription, AKS resource and node pools, API networking, managed Entra and AKS authorization, OIDC/workload identity, Azure identities, storage and registry services, and provider GPU/CSI enablement. It does not install Kueue, KubeRay, Tau controllers, queue policy, workspaces, Kubernetes RBAC, or workload objects. Those begin in [TauGrid setup](../taugrid-setup/) after the AKS handoff gate passes.

The subscription gate is the same whether the cluster is created with Terraform, Bicep, an ARM JSON template, Azure Developer CLI (`azd`), Azure CLI, the Azure portal, or an existing platform pipeline.

## 0. Obtain an approved subscription

If you are evaluating independently and do not have Azure, start with an [Azure free account](https://azure.microsoft.com/free/). Confirm the spending limit, available services, and regional quota before attempting a GPU cluster.

In an organization, request a subscription through the approved cloud-platform or landing-zone process. Record the billing owner, technical owner, tenant, management group, policy assignments, network connectivity, allowed regions, cost center, and incident contact. Do not create a personal subscription for organization workloads or bypass subscription vending and policy controls.

## 1. Select the subscription explicitly

For an interactive operator session:

```bash
az login
az account list --output table
az account set --subscription "<subscription-id>"

SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
TENANT_ID="$(az account show --query tenantId -o tsv)"
LOCATION="<azure-region>"
```

Confirm that `SUBSCRIPTION_ID`, `TENANT_ID`, and `LOCATION` identify the intended environment before creating anything. Automation should use an approved OIDC, service-principal, or managed-identity context rather than a developer login. Do not store client secrets, access tokens, account keys, or SAS tokens in the repository.

## 2. Check permissions and resource providers

The provisioning identity needs permission to create the selected resources at the intended resource-group scope. If the deployment creates Azure role assignments, `Contributor` alone is insufficient; the identity also needs an approved role-assignment permission such as `User Access Administrator` or a purpose-built custom role at the narrowest practical scope.

Register the baseline providers used by a typical AKS environment:

```bash
for namespace in \
  Microsoft.ContainerService \
  Microsoft.Compute \
  Microsoft.Network \
  Microsoft.ManagedIdentity \
  Microsoft.Storage \
  Microsoft.ContainerRegistry
do
  az provider register --namespace "$namespace"
done

az provider list \
  --query "[?namespace=='Microsoft.ContainerService' || namespace=='Microsoft.Compute' || namespace=='Microsoft.Network' || namespace=='Microsoft.ManagedIdentity' || namespace=='Microsoft.Storage' || namespace=='Microsoft.ContainerRegistry'].[namespace,registrationState]" \
  --output table
```

Add providers only for services the selected design uses, such as `Microsoft.KeyVault`, `Microsoft.Insights`, or `Microsoft.OperationalInsights`. Provider registration is subscription-scoped; do not register unrelated providers speculatively.

## 3. Check region, quota, and cost before GPU deployment

Verify that the target region supports the intended AKS and VM SKUs, then inspect regional compute usage:

```bash
az vm list-usage --location "$LOCATION" --output table
```

Check both total regional vCPU quota and the VM-family quota for every planned node pool. GPU availability and quota are subscription-and-region specific. Request quota before deployment, or begin with a CPU-only cluster and add the GPU pool later.

Create a budget and alerts before provisioning persistent GPU capacity. A budget does not stop resources automatically; it supplies an early warning. Use explicit minimum and maximum node counts, and retain a documented cleanup or `destroy` path for every sandbox.

## 4. Choose a provisioning path

All paths must produce the same output contract; they differ in ownership and change management.

| Path | Best fit | What to retain | Start here |
| --- | --- | --- | --- |
| **Terraform** | Reproducible open-source environments and teams already using Terraform | Reviewed plan, protected remote state, provider lock file, non-secret variables, and destroy procedure | [TauGrid AKS Terraform](https://github.com/Azure/taugrid/tree/main/terraform/aks) and the [official AKS Terraform quickstart](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-terraform) |
| **Bicep** | Azure-native infrastructure as code with concise authoring and ARM what-if | Bicep modules, parameter files, what-if output, and deployment history | [Official AKS Bicep quickstart](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-bicep) |
| **ARM JSON template** | Existing ARM, EV2, or deployment-spec pipelines | Template, parameter files, what-if output, and deployment history | Compile reviewed Bicep to ARM JSON or adapt the repository's `ev2/templates/aks-cluster.template.json`; the EV2 template is a production reference, not a drop-in quickstart |
| **Azure Developer CLI (`azd`)** | A developer-facing template that packages infrastructure and application setup | `azure.yaml`, AKS-capable Bicep or Terraform source, environment inputs, and pipeline configuration | [Official AKS `azd` quickstart](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-azd) |
| **Azure CLI** | A bounded sandbox or learning walkthrough | Script, explicit arguments, resource inventory, and cleanup commands | [Official AKS CLI quickstart](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-cli) |
| **Azure portal** | First exploration or an organization-required guided workflow | Exported template, final settings, resource inventory, and cleanup owner | [Official AKS portal quickstart](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-portal) |
| **Existing AKS cluster** | A platform team already owns networking, policy, and cluster lifecycle | Ownership record, cluster identifiers, supported capabilities, and change path | Skip creation and validate the common output contract below |

### Terraform

Use the repository's GPU-enabled AKS Terraform root when the environment should
be reproducible outside an Azure-specific release system. It creates AKS with
local administrator credentials by default, OIDC workload identity, a GPU node pool, and the
optional ADX, adx-mon, and Portal integration. Then it installs the released
TauGrid chart through `tau cluster install`.

Clone the repository, create an untracked parameter file, and review before
applying:

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid/terraform/aks
cp terraform.tfvars.example terraform.tfvars
terraform init
terraform plan -out tau-aks.tfplan
terraform apply tau-aks.tfplan
```

Set `subscription_id` and the GPU SKU and labels in `terraform.tfvars`. To use
managed Entra AKS authentication, set the optional Entra administrator group
inputs. For WSL, Linux, and macOS, set
`command_interpreter = ["bash", "-c"]`. Set `enable_adx = true` only when the
deployment should create the billable ADX data plane and install adx-mon.

Use remote state with locking for shared environments. The tracked
`.terraform.lock.hcl` pins provider versions and must remain committed; do not
copy it into a local parameter file.

Terraform state, saved plans, and secret-bearing variable files can contain sensitive values even when terminal output is redacted. Do not commit them. Store shared state in an encrypted, access-controlled backend and protect saved plan artifacts with equivalent access and retention controls.

### Azure Developer CLI

`azd` requires an `azure.yaml` project backed by Bicep or Terraform; Tau does not provide an `azd` template. The official AKS quickstart supplies an AKS-capable sample:

```bash
azd init --template Azure-Samples/aks-store-demo
azd auth login
azd up
```

That template deploys its own sample application, not Tau, Kueue, or a TauWorkspace. Use it to learn the `azd` lifecycle, or build an organization template whose infrastructure produces the output contract below. Remove the sample environment when finished:

```bash
azd down
```

### Bicep

Use Bicep when Azure Resource Manager is the deployment authority:

The target resource group must already exist. In a governed environment, obtain it through the platform's approved resource-group or landing-zone process:

```bash
RESOURCE_GROUP="<approved-resource-group>"
az group show --name "$RESOURCE_GROUP" --output table
```

For an independent sandbox only, create and own the cleanup of a dedicated group:

```bash
RESOURCE_GROUP="tau-sandbox"
az group create --name "$RESOURCE_GROUP" --location "$LOCATION"
```

Then preview and deploy:

```bash
az deployment group what-if \
  --resource-group "$RESOURCE_GROUP" \
  --template-file main.bicep \
  --parameters @parameters.json

az deployment group create \
  --resource-group "$RESOURCE_GROUP" \
  --template-file main.bicep \
  --parameters @parameters.json
```

Keep reusable concerns such as network, AKS, registry, storage, identity, and monitoring in separate modules. Review the `what-if` result before deployment.

### ARM JSON

ARM JSON uses the same Resource Manager deployment boundary as Bicep:

```bash
az deployment group what-if \
  --resource-group "$RESOURCE_GROUP" \
  --template-file azuredeploy.json \
  --parameters @azuredeploy.parameters.json

az deployment group create \
  --resource-group "$RESOURCE_GROUP" \
  --template-file azuredeploy.json \
  --parameters @azuredeploy.parameters.json
```

Prefer Bicep for new human-authored templates and emit ARM JSON when an existing pipeline requires it. The repository's EV2 artifacts demonstrate production ARM orchestration but include service-specific assumptions and must not be presented as a generic one-command Tau installer.

### Azure CLI or portal

CLI and portal are useful for learning the Azure resource shape. After using them, do not leave a durable environment without an owner or reproducible configuration. After validating a sandbox, move the accepted settings into the team's Terraform, Bicep/ARM, or platform pipeline.

## 5. Require the same AKS handoff contract from every path

Before TauGrid enablement, record these non-secret AKS values. Kubernetes queue policy, StorageClasses/PVCs, workspaces, and TauGrid observability are later setup outputs and are not evidence that AKS setup itself is complete.

| Output | Why Tau onboarding needs it |
| --- | --- |
| Subscription ID and tenant ID | Select the Azure authority and authenticate the researcher connection |
| Region | Check SKU availability, quota, data locality, and cost |
| AKS resource group and cluster name | Acquire normal cluster-user credentials and identify the platform owner |
| Managed Entra and AKS authorization mode | Ensure normal cluster-user credentials and workspace authorization can work |
| Private-cluster and network access requirements | Explain how operators and researchers reach the Kubernetes API |
| OIDC issuer and workload-identity status | Federate workload ServiceAccounts without stored credentials |
| Registry hostname and image ownership | Resolve immutable workload images and pull authorization |
| Azure storage service, identity, network path, CSI driver, and owner | Supply the provider capability that a later Kubernetes storage contract consumes |
| GPU driver/device registration path and owner | Ensure required GPU resources can become schedulable before Tau validates them |
| IaC repository, state/deployment record, and cleanup owner | Make Azure updates, rollback, and teardown reproducible |

For an existing cluster, inspect the Azure identity features before continuing:

```bash
AKS_RESOURCE_GROUP="<aks-resource-group>"
AKS_CLUSTER="<aks-cluster-name>"
AKS_CONTEXT="<aks-context>"

az aks show \
  --subscription "$SUBSCRIPTION_ID" \
  --resource-group "$AKS_RESOURCE_GROUP" \
  --name "$AKS_CLUSTER" \
  --query "{managedEntra:aadProfile.managed,tenantID:aadProfile.tenantId,azureRBAC:aadProfile.enableAzureRbac,localAccountsDisabled:disableLocalAccounts,oidc:oidcIssuerProfile.issuerUrl,workloadIdentity:securityProfile.workloadIdentity.enabled,privateCluster:apiServerAccessProfile.enablePrivateCluster}" \
  --output yaml
```

You should see `managedEntra: true` and the expected tenant and authorization mode. `localAccountsDisabled` can be either `true` or `false`; TauGrid does not require local accounts to be disabled. OIDC, workload identity, private-cluster, GPU, and CSI settings must match the capabilities the intended workloads need.

Obtain normal cluster-user credentials and prove that the operator context reaches the Kubernetes API:

```bash
az aks get-credentials \
  --subscription "$SUBSCRIPTION_ID" \
  --resource-group "$AKS_RESOURCE_GROUP" \
  --name "$AKS_CLUSTER" \
  --context "$AKS_CONTEXT" \
  --overwrite-existing

kubectl --context "$AKS_CONTEXT" cluster-info
kubectl --context "$AKS_CONTEXT" get nodes
```

You should see the intended AKS API endpoint and the required nodes in `Ready` state. Managed-Entra credentials require `kubelogin` on the operator workstation. Private clusters also require the documented VPN, peering, private DNS, or approved jump-host path before these commands can pass.

## 6. End AKS setup; begin TauGrid setup

AKS setup is complete only when:

- the intended subscription, tenant, region, resource group, and AKS resource are recorded;
- a normal managed-Entra cluster-user identity can obtain credentials and reach the API through the required network path;
- required node pools are `Ready`, with provider GPU and CSI capabilities enabled when the target workloads need them; and
- Azure quota, identity, registry, storage, cost, and cleanup ownership are explicit.

An AKS cluster that passes this provider gate is still not a Tau-enabled cluster. Continue in this order:

1. Complete [TauGrid setup](../taugrid-setup/) to install the released MCR Helm chart with the Tau CLI and validate the Kubernetes control plane.
2. [Enable a workspace](../../tasks/platform/enable-workspace/) to create the TauWorkspace and its namespace, LocalQueue, RBAC, ServiceAccount, and platform-owned storage contract.
3. [Hand off the workspace](../../tasks/platform/handoff/) only after its readiness check passes.

## Official Azure references

- [Azure resource providers and registration](https://learn.microsoft.com/azure/azure-resource-manager/management/resource-providers-and-types)
- [AKS limits, SKUs, and regions](https://learn.microsoft.com/azure/aks/quotas-skus-regions)
- [AKS cluster authentication](https://learn.microsoft.com/azure/aks/concepts-cluster-authentication)
- [Create and manage Azure budgets](https://learn.microsoft.com/azure/cost-management-billing/costs/tutorial-acm-create-budgets)
