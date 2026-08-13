---
title: Prepare ADX/Kusto for TauGrid
weight: 3
description: Provision and authorize the optional Azure Data Explorer data plane used by TauGrid integrations
---

{{< maturity status="alpha" reviewed="2026-08-13" >}}

ADX/Kusto is an optional platform data service. TauGrid does not create its
cluster, databases, Entra identities, federation, or database roles. Prepare
it once; Portal, lifecycle recorder, and adx-mon are separate consumers.

## Provision the service

Choose an approved region, SKU, capacity, network path, retention policy, and
cost owner. This CLI shape is not a universal SKU recommendation:

```bash
az extension add --name kusto
az kusto cluster create --resource-group <resource-group> \
  --name <adx-cluster-name> --location <region> \
  --sku name=<approved-sku> tier=<approved-tier> capacity=<instance-count> \
  --enable-streaming-ingest false
for db in Metrics Logs CostTracking Audit; do
  az kusto database create --resource-group <resource-group> \
    --cluster-name <adx-cluster-name> --database-name "$db" \
    --read-write-database location=<region> \
    soft-delete-period=P31D hot-cache-period=P7D
done
```

`kind=ReadWrite` is not accepted by current Azure CLI Kusto extensions; a
read-write database is implied by `--read-write-database`.

`Metrics` is needed by Portal Kusto views and lifecycle history. `Logs`,
`CostTracking`, and `Audit` are required only for the corresponding adx-mon
pipelines.

## Create identities and grant roles

For AKS, federate each exact ServiceAccount to a managed identity. Workload
Identity obtains an Entra token; it does not grant ADX database access.

```bash
az identity federated-credential create --resource-group <resource-group> \
  --identity-name <identity-name> --name <credential-name> \
  --issuer <aks-oidc-issuer> \
  --subject system:serviceaccount:<namespace>:<service-account> \
  --audiences api://AzureADTokenExchange
```

An authorized ADX administrator must separately grant database roles. In
production, use distinct principals: schema deployer (table/mapping/function
management), Portal reader (`Viewer`), lifecycle recorder writer (`Ingestor`),
and adx-mon identity (the current chart's documented management privileges).
Azure RBAC, Kubernetes RBAC, and ADX database roles are independent.

For example, an ADX administrator can grant the two least-privilege consumer
roles in PowerShell or another approved Azure CLI environment:

```powershell
$tenant = az account show --query tenantId -o tsv

az kusto database-principal-assignment create `
  --resource-group <resource-group> --cluster-name <adx-cluster-name> `
  --database-name Metrics --principal-assignment-name taugrid-portal-viewer `
  --principal-id <portal-managed-identity-client-id> --principal-type App `
  --role Viewer --tenant-id $tenant

az kusto database-principal-assignment create `
  --resource-group <resource-group> --cluster-name <adx-cluster-name> `
  --database-name Metrics --principal-assignment-name taugrid-recorder-ingestor `
  --principal-id <recorder-managed-identity-client-id> --principal-type App `
  --role Ingestor --tenant-id $tenant
```

Use a stable, unique assignment name per database/principal/role. These
commands grant ADX data-plane access only; they do not create the federated
credential or Kubernetes RBAC.

## Generate, execute, and verify producer schemas

Tables are not created implicitly by Portal or lifecycle recorder. Apply each
enabled producer's schema before it writes. Durable Ray history needs
`Metrics.TauExpRunLifecycle`, its mapping, and function.

### Step 1: Generate and review the KQL artifact

The schema/release owner runs the matching `taugrid-portal` release tool and
stores the generated artifact with the deployment inputs:

```bash
taugrid-portal exp kusto schema --ingestion lifecycle \
  > lifecycle-schema.kql
```

This is a pure text generator: it does not contact ADX, does not need a
kubeconfig, and does not accept endpoint or database arguments. It must run
from release engineering or the schema pipeline, not from a Portal Pod or the
lifecycle recorder. The output is idempotent KQL; its essential shape is:

```kusto
.create-merge table TauExpRunLifecycle (...)
.create-or-alter function TauExpRunLifecycleDashboardRows() { ... }
// plus the TauExpRunLifecycle JSON ingestion mapping
```

Review and deploy the complete generated file, never this abbreviated example.

### Step 2: Execute the KQL in ADX

A schema-deployment identity executes `lifecycle-schema.kql` against the
**Metrics** database. For a guided/manual deployment, open the target ADX
cluster in Azure Portal, select **Query**, choose the `Metrics` database, paste
the complete file into a new query tab, and select **Run**. For production,
commit the artifact to the approved IaC/KQL deployment pipeline and run that
same file as the schema principal.

### Step 3: Verify the applied objects

In the same `Metrics` query tab, run:

```kusto
.show table TauExpRunLifecycle schema as json
.show table TauExpRunLifecycle ingestion mappings
```

The emitted KQL is idempotent, so the reviewed schema deployment may run it
again on an upgrade. Stellar uses the scalar metric tables supplied by its
selected ingestion path (commonly adx-mon remote-write
`Metrics.ExperimentMetrics`).
For adx-mon, follow its published chart guide and enable Metrics/Logs table
precreation before broad collection to avoid ADX control-plane throttling.

Only hand consumers a tested endpoint, database, ServiceAccount subject, and
non-secret identity client ID. Then configure [Portal](../enable-portal/) or
adx-mon.
