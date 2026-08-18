# adx-mon Helm Chart

Azure Data Explorer Monitor is an observability pipeline for GPU-enabled
Kubernetes clusters. It collects metrics, logs, and GPU telemetry and ships them
to Azure Data Explorer (ADX) for dashboarding, alerting, and cost tracking.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  AKS Cluster                                                    │
│                                                                 │
│  ┌──────────────┐  ┌───────────────────┐  ┌──────────────────┐ │
│  │  Collector    │  │ Collector         │  │ kube-state-      │ │
│  │  (DaemonSet)  │  │ Singleton         │  │ metrics          │ │
│  │  per-node     │  │ (Deployment)      │  │ (Deployment)     │ │
│  │  - DCGM       │  │ - API server      │  │ - pod/node       │ │
│  │  - journal    │  │   metrics         │  │ - Kueue CRDs     │ │
│  │  - kernel     │  └────────┬──────────┘  └────────┬─────────┘ │
│  └───────┬───────┘           │                      │           │
│          │                   │    ┌─────────────────┘           │
│          └───────────┬───────┘    │                             │
│                      ▼            ▼                             │
│              ┌───────────────────────────┐                      │
│              │  Ingestor (StatefulSet)   │                      │
│              │  batches + ships to ADX   │                      │
│              └────────────┬──────────────┘                      │
│                           │                                     │
│  ┌──────────────┐         │        ┌──────────────────────────┐ │
│  │  Operator     │        │        │  Alerter (Deployment)    │ │
│  │  CRD control  │        │        │  KQL alert evaluation    │ │
│  └──────────────┘         │        └──────────────────────────┘ │
└───────────────────────────┼─────────────────────────────────────┘
                            ▼
                ┌───────────────────────┐
                │  Azure Data Explorer  │
                │  ┌─────────────────┐  │
                │  │ Metrics DB      │  │
                │  │ Logs DB         │  │
                │  │ CostTracking DB │  │
                │  │ Audit DB        │  │
                │  └─────────────────┘  │
                └───────────────────────┘
```

## Prerequisites

| Requirement | Details |
|-------------|---------|
| Kubernetes | >= 1.28.0 |
| Helm | >= 3.12 |
| ADX cluster | With databases: `Metrics`, `Logs`, `CostTracking`, `Audit` |
| Managed Identity | User-assigned or system-assigned MI with ADX `Database Admin` on each database (`Metrics`, `Logs`, `CostTracking`, `Audit`) |
| AKS tier | **Standard or Premium** (Free tier does not emit audit logs) |
| GPU nodes (optional) | DCGM exporter deployed for GPU telemetry |
| Kueue (optional) | For job scheduling metrics |

## Quick Start

```bash
# Add required values
cat > my-values.yaml <<EOF
adx:
  clusterUrl: "https://mycluster.westus2.kusto.windows.net"
  clientId: "<managed-identity-client-id>"
  workloadIdentity:
    enabled: true
global:
  clusterName: "my-cluster"
  region: "westus2"
EOF

# Install with the GPU cluster preset
helm install adx-mon charts/adx-mon/ \
  -n adx-mon --create-namespace \
  -f charts/adx-mon/values.yaml \
  -f charts/adx-mon/values-ai-runtime.yaml \
  -f my-values.yaml

# Verify
kubectl get pods -n adx-mon
kubectl get functions,alertrules,summaryrules -n adx-mon
```

## Value Files

| File | Purpose |
|------|---------|
| `values.yaml` | Base defaults — all features disabled or minimal |
| `values-ai-runtime.yaml` | GPU cluster preset - GPU telemetry, journal logs, drop-metrics filter, management policies |
| `values-dev.yaml` | Development overrides |

Apply in order: `values.yaml` → `values-ai-runtime.yaml` → cluster-specific overrides.

## Configuration Reference

### Global Settings

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.namespace` | Namespace override | Release namespace |
| `global.clusterName` | Cluster name label on all telemetry | `""` |
| `global.region` | Region label on all telemetry | `""` |
| `global.imagePullPolicy` | Image pull policy | `IfNotPresent` |
| `global.imagePullSecrets` | Image pull secret names | `[]` |

### ADX Connection

| Parameter | Description | Default |
|-----------|-------------|---------|
| `adx.clusterUrl` | ADX cluster URL | `""` |
| `adx.clientId` | Managed Identity client ID for ADX auth | `""` |
| `adx.workloadIdentity.enabled` | Add AKS Azure Workload Identity annotations and labels to ingestor and alerter | `false` |
| `adx.metricsDatabase` | Metrics database name | `Metrics` |
| `adx.logsDatabase` | Logs database name | `Logs` |
| `adx.costTrackingDatabase` | Cost tracking database name | `CostTracking` |
| `adx.auditDatabase` | Audit database name | `Audit` |
| `adx.configMapRef` | Pre-existing ConfigMap name with ADX config (ArgoCD mode) | `""` |

> **ConfigMap mode:** When `adx.configMapRef` is set and `adx.clusterUrl` is empty, ingestor/alerter pods read `ADX_CLUSTER_URL` and `ADX_CLIENT_ID` from the named ConfigMap.
>
> **Cluster labels:** `global.clusterName` must still resolve at Helm render time. For normal ArgoCD deployments, set it explicitly in the per-cluster overlay. For Helm install/upgrade flows, the chart also falls back to `ADX_CLUSTER_NAME` from the referenced ConfigMap via Helm `lookup`. The collector does **not** expand `$(...)` variables inside `config.toml`.
>
> **Important:** When using kubelet managed identity (no explicit client ID), omit `adx.clientId` entirely — do not set it to `""`. The Azure SDK treats an empty string as an invalid client ID.
>
> **AKS Workload Identity:** Set `adx.workloadIdentity.enabled: true` together with
> `adx.clientId` when the ingestor and alerter authenticate to ADX through an AKS
> federated identity. The chart then annotates their ServiceAccounts with
> `azure.workload.identity/client-id` and labels their Pods with
> `azure.workload.identity/use: "true"`.

### Operator

| Parameter | Description | Default |
|-----------|-------------|---------|
| `operator.enabled` | Deploy operator | `true` |
| `operator.image.repository` | Image repository | `mcr.microsoft.com/aks/ai-runtime/adx-mon/operator` |
| `operator.image.tag` | Image tag (defaults to `appVersion`) | `""` |
| `operator.replicas` | Replica count | `1` |
| `operator.resources` | CPU/memory requests and limits | 100m/128Mi req, 500m/512Mi lim |
| `operator.tolerations` | Pod tolerations | `CriticalAddonsOnly` |
| `operator.nodeSelector` | Node selector | `{}` |

### Collector (DaemonSet)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `collector.enabled` | Deploy collector DaemonSet | `true` |
| `collector.image.repository` | Image repository | `mcr.microsoft.com/aks/ai-runtime/adx-mon/collector` |
| `collector.image.tag` | Image tag | `""` (appVersion) |
| `collector.hostPort` | Host port for Prometheus remote write | `3100` |
| `collector.storageDir` | WAL storage directory on host | `/mnt/collector` |
| `collector.walFlushIntervalMs` | WAL flush interval (ms) | `1000` |
| `collector.maxBatchSize` | Max batch size for metric samples | `10000` |
| `collector.maxConnections` | Max connections | `100` |
| `collector.insecureSkipVerify` | Skip TLS verify to ingestor | `true` |
| `collector.resources` | CPU/memory requests and limits | 50m/100Mi req, 500m/2Gi lim |
| `collector.tolerations` | Pod tolerations | CriticalAddonsOnly + GPU taints |

#### Prometheus Scrape

| Parameter | Description | Default |
|-----------|-------------|---------|
| `collector.prometheusScrape.enabled` | Enable Prometheus scraping | `true` |
| `collector.prometheusScrape.scrapeIntervalSeconds` | Scrape interval | `30` |
| `collector.prometheusScrape.scrapeTimeoutSeconds` | Scrape timeout | `25` |
| `collector.prometheusScrape.extraStaticTargets` | Additional node-local static scrape targets | `[]` |
| `collector.prometheusScrape.keepMetrics` | Regex allowlist for metrics | `[]` (keep all) |
| `collector.prometheusScrape.dropMetrics` | Regex denylist for metrics | `[]` |
| `collector.prometheusScrape.scrapeDropMetricsExclude` | Families to retain only on node-local scrapes | `[]` |

> **Tip:** The `values-ai-runtime.yaml` preset includes 23 `dropMetrics` regex patterns that reduce ADX table count from ~1074 to ~200, preventing ingestion saturation.

#### Journal Logs

| Parameter | Description | Default |
|-----------|-------------|---------|
| `collector.journalLogs.enabled` | Enable journal log collection | `true` |
| `collector.journalLogs.targets` | Journal match rules with database/table routing | Kubelet only |

#### GPU Telemetry

| Parameter | Description | Default |
|-----------|-------------|---------|
| `collector.gpu.dcgmScrape` | Scrape DCGM exporter via annotation discovery | `true` |
| `collector.gpu.kernelTarget` | Collect kernel logs via journald `_TRANSPORT=kernel` | `true` |
| `collector.gpu.kernelLogTable` | ADX table for kernel logs | `KernelLogs` |

### Collector Singleton

| Parameter | Description | Default |
|-----------|-------------|---------|
| `collectorSingleton.enabled` | Deploy singleton collector | `true` |
| `collectorSingleton.scrapeTargets` | Cluster-global scrape targets, collected once per cluster | `[]` |
| `collectorSingleton.scrapeDropMetricsExclude` | Families retained for cluster-global scrapes | controller-runtime, workqueue, leader-election |

### Ingestor (StatefulSet)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `ingestor.enabled` | Deploy ingestor | `true` |
| `ingestor.image.repository` | Image repository | `mcr.microsoft.com/aks/ai-runtime/adx-mon/ingestor` |
| `ingestor.replicas` | Replica count | `1` |
| `ingestor.storageDir` | WAL storage directory | `/mnt/ingestor` |
| `ingestor.maxSegmentAge` | Max segment age before flush | `5s` |
| `ingestor.maxDiskUsage` | Max WAL disk usage (bytes) | `21474836480` (20 GiB) |
| `ingestor.maxTransferSize` | Max transfer batch size (bytes) | `10485760` (10 MiB) |
| `ingestor.maxConnections` | Max inbound connections | `1000` |
| `ingestor.resources` | CPU/memory requests and limits | 200m/256Mi req, 2/4Gi lim |

### Alerter

| Parameter | Description | Default |
|-----------|-------------|---------|
| `alerter.enabled` | Deploy alerter | `true` |
| `alerter.image.repository` | Image repository | `mcr.microsoft.com/aks/ai-runtime/adx-mon/alerter` |
| `alerter.replicas` | Replica count | `1` |
| `alerter.alerterEndpoint` | Webhook endpoint for alert notifications | `""` |
| `alerter.resources` | CPU/memory requests and limits | 100m/125Mi req, 400m/1400Mi lim |

### kube-state-metrics

| Parameter | Description | Default |
|-----------|-------------|---------|
| `kube-state-metrics.enabled` | Deploy KSM subchart | `true` |
| `kube-state-metrics.shards` | Number of KSM shards | `1` |
| `kube-state-metrics.customResourceState.enabled` | Enable Kueue CRD metrics | `true` |
| `kube-state-metrics.podAnnotations` | Annotations for collector scrape discovery | `adx-mon/scrape: "true"` |

> **Image source:** KSM defaults to `mcr.microsoft.com/oss/v2/kubernetes/kube-state-metrics:v2.19.1` — a dalec-built MCR image (MSFT Go, `runAsNonRoot` compatible, active CVE patching). No ACR mirror required.

### Functions (KQL Views)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `functions.enabled` | Deploy Function CRDs | `true` |
| `functions.items.<name>.enabled` | Enable individual function | `true` |
| `functions.items.<name>.database` | Target ADX database | varies |

Available functions: `gpuHealth`, `nodeHealth`, `containerMetrics`, `kueueMetrics`, `ncclErrors`, `xidErrors`, `trainingJobSummary`, `experimentMetricsDashboardRows`.

### AlertRules

| Parameter | Description | Default |
|-----------|-------------|---------|
| `alertRules.enabled` | Deploy AlertRule CRDs | `true` |
| `alertRules.items.<name>.enabled` | Enable individual alert | `true` |
| `alertRules.items.<name>.interval` | Evaluation interval | varies |
| `alertRules.items.<name>.destination` | Webhook destination | `""` |

Available alerts: `gpuXidCritical`, `nodeNotReady`, `eccUncorrectable`, `nvlinkDown`, `jobFailureRate`.

### SummaryRules (Cost Rollups)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `summaryRules.enabled` | Deploy SummaryRule CRDs | `true` |
| `summaryRules.items.<name>.enabled` | Enable individual rule | `true` |
| `summaryRules.items.<name>.interval` | Rollup interval | varies |

Available rules: `gpuUtilizationHourly`, `gpuCostHourly`, `gpuCostDaily`, `namespaceCostMonthly`, `nodeResourcesHourly`.

### GPU Cost Rates

| Parameter | Description | Default |
|-----------|-------------|---------|
| `cost.gpuRates` | Per-GPU-hour rates (USD) by VM SKU | See `values.yaml` |

> **Important:** These are **per-GPU-hour** rates, not per-VM-hour. The KQL query multiplies `gpu_count × rate_per_gpu_hour`, so using VM-level prices would overcount by the GPU count. Override per cluster for internal transfer pricing.

### ManagementCommands

| Parameter | Description | Default |
|-----------|-------------|---------|
| `managementCommands.precreateMetricsTables.enabled` | Pre-create metrics tables | `false` |
| `managementCommands.precreateLogsTables.enabled` | Pre-create log tables | `false` |
| `managementCommands.ingestionBatching.enabled` | Set ingestion batching policies | `false` |
| `managementCommands.retentionPolicies.enabled` | Set retention/caching policies | `false` |

> **Why pre-create tables?** Without pre-creation, adx-mon auto-creates tables on first metric encounter. With 300+ concurrent creates, this exceeds the ADX control plane limit (20 commands/window), causing a 10–20 min data blackout on first deploy.

### Security Context

| Parameter | Description | Default |
|-----------|-------------|---------|
| `podSecurityContext.runAsNonRoot` | Run as non-root | `false` (upstream images run as root) |
| `podSecurityContext.supplementalGroups` | Additional GIDs | `[0, 23, 101]` |
| `podSecurityContext.seccompProfile.type` | Seccomp profile | `RuntimeDefault` |
| `containerSecurityContext.allowPrivilegeEscalation` | Privilege escalation | `false` |
| `containerSecurityContext.readOnlyRootFilesystem` | Read-only root FS | `true` |

GID explanation:
- `0` — read container log files (`/var/log/pods/*.log` is `root:root 640`)
- `23` — read journal files on Ubuntu (`systemd-journal` GID)
- `101` — read journal files on Azure Linux / Mariner (`systemd-journal` GID)

## Scale Profiles

| Profile | Nodes | Ingestor Replicas | KSM Shards | Notes |
|---------|-------|-------------------|------------|-------|
| Small | 1–50 | 1 | 1 | Default values |
| Medium | 50–200 | 2 | 1 | Increase ingestor replicas |
| Large | 200–1000 | 3 | 2 | Add KSM shards |
| XL | 1000+ | 5 | 4 | Consider dedicated node pool |

```yaml
# Medium profile example
ingestor:
  replicas: 2
  resources:
    limits:
      cpu: "4"
      memory: 8Gi
```

## Audit Log Pipeline (Optional)

Kubernetes audit logs live in the ADX `Audit` database. This data is delivered through an external pipeline — **not** through the adx-mon collector.

### Architecture

```
AKS Cluster (Standard/Premium tier)
  → Diagnostic Settings (kube-audit-admin category)
    → Event Hub (same region as AKS)
      → ADX Data Connection (MULTIJSON format)
        → Audit DB / KubeAuditLogs table
          → AuditEvents() Function (KQL view)
```

### Prerequisites

The audit pipeline requires infrastructure provisioned outside this Helm chart. For automated setup via EV2, see [WI-26](https://github.com/Azure/taugrid/issues/194).

| Resource | Requirement |
|----------|-------------|
| AKS tier | **Standard or Premium** — Free tier silently accepts diagnostic settings but never emits logs |
| Event Hub namespace | Must be in the **same region** as the AKS cluster |
| Event Hub | Dedicated hub (e.g., `kube-audit`) with ≥ 4 partitions |
| ADX managed identity | System-assigned MI enabled on the ADX cluster |
| RBAC | ADX MI granted `Azure Event Hubs Data Receiver` on the Event Hub **namespace** (not on the ADX cluster) |
| ADX data connection | Format must be **MULTIJSON** (not JSON) |

### ⚠️ Critical Gotchas

1. **MULTIJSON, not JSON.** AKS wraps audit logs in `{"records": [...]}`. Using `JSON` format causes ADX to treat the entire envelope as one record — fields don't match, and you get **silent 0-row ingestion** (no errors, no failures, just no data).

2. **Standard or Premium tier only.** AKS Free tier accepts diagnostic settings without error but never emits `kube-audit` or `kube-audit-admin` events. There is no warning. Upgrade with `az aks update --tier standard`.

3. **Same region requirement.** The Event Hub namespace must be in the same Azure region as the AKS cluster. Cross-region delivery is not supported for diagnostic settings.

4. **Never delete/recreate diagnostic settings.** Deleting and recreating causes a 30–90 minute delivery gap with no error indication. Always update in place using `--overwrite` or modify the existing setting.

5. **RBAC scope matters.** The ADX managed identity needs the `Azure Event Hubs Data Receiver` role on the Event Hub **namespace**, not on the Event Hub itself or on the ADX cluster. The Azure portal's default scope suggestion is misleading.

6. **Propagation delay.** First-time diagnostic settings take 5–15 minutes to start emitting. This is normal. Expect 800–1,400 rows/hour for a moderately active cluster.

### Manual Setup (for teams not using EV2)

```bash
# Variables — adjust for your environment
AKS_RG="my-aks-resource-group"
AKS_NAME="my-aks-cluster"
REGION="westus2"
EH_NAMESPACE="my-audit-eh-namespace"
EH_NAME="kube-audit"
ADX_CLUSTER="my-adx-cluster"
ADX_RG="my-adx-resource-group"
ADX_DB="Audit"

# 1. Create Event Hub namespace + hub (same region as AKS)
az eventhubs namespace create \
  --name $EH_NAMESPACE \
  --resource-group $AKS_RG \
  --location $REGION \
  --sku Standard
az eventhubs eventhub create \
  --name $EH_NAME \
  --namespace-name $EH_NAMESPACE \
  --resource-group $AKS_RG \
  --partition-count 4

# 2. Ensure AKS cluster is Standard tier (Free tier won't emit audit logs)
az aks update --resource-group $AKS_RG --name $AKS_NAME --tier standard

# 3. Create diagnostic setting (kube-audit-admin → Event Hub)
AKS_ID=$(az aks show -g $AKS_RG -n $AKS_NAME --query id -o tsv)
EH_RULE_ID=$(az eventhubs namespace authorization-rule show \
  --namespace-name $EH_NAMESPACE -g $AKS_RG \
  -n RootManageSharedAccessKey --query id -o tsv)
az monitor diagnostic-settings create \
  --name "kube-audit-to-eventhub" \
  --resource "$AKS_ID" \
  --event-hub "$EH_NAME" \
  --event-hub-rule "$EH_RULE_ID" \
  --logs '[{"category":"kube-audit-admin","enabled":true}]'

# 4. Enable ADX system-assigned managed identity
az kusto cluster update \
  --name $ADX_CLUSTER \
  --resource-group $ADX_RG \
  --type SystemAssigned

# 5. Grant ADX MI "Azure Event Hubs Data Receiver" on Event Hub NAMESPACE
ADX_MI=$(az kusto cluster show -n $ADX_CLUSTER -g $ADX_RG \
  --query identity.principalId -o tsv)
EH_NS_ID=$(az eventhubs namespace show \
  --name $EH_NAMESPACE -g $AKS_RG --query id -o tsv)
az role assignment create \
  --assignee $ADX_MI \
  --role "Azure Event Hubs Data Receiver" \
  --scope $EH_NS_ID

# 6. Create ADX table
# In ADX query editor:
#   .create-merge table KubeAuditLogs (records: dynamic)

# 7. Create ADX data connection (MULTIJSON format!)
az kusto data-connection event-hub create \
  --cluster-name $ADX_CLUSTER \
  --resource-group $ADX_RG \
  --database-name $ADX_DB \
  --data-connection-name "kube-audit-ingestion" \
  --event-hub-resource-id "$EH_NS_ID/eventhubs/$EH_NAME" \
  --consumer-group '$Default' \
  --managed-identity-resource-id \
    "/subscriptions/<sub-id>/resourceGroups/$ADX_RG/providers/Microsoft.Kusto/clusters/$ADX_CLUSTER" \
  --data-format MULTIJSON \
  --table-name KubeAuditLogs \
  --mapping-rule-name ""
```

### Verification

```kql
-- Check row count
KubeAuditLogs | count

-- Parse and query audit events
KubeAuditLogs
| mv-expand record = parse_json(records)
| extend log = parse_json(tostring(record.properties.log))
| project
    Timestamp = todatetime(record['time']),
    Verb = tostring(log.verb),
    Resource = tostring(log.objectRef.resource),
    Namespace = tostring(log.objectRef.namespace),
    User = tostring(log.user.username)
| take 10

-- Or use the pre-built Function (once enabled and audit data is flowing):
AuditEvents() | take 10 | project Timestamp, Verb, Resource, Namespace, User
```

> **Note on `AuditEvents()`:** The `AuditEvents()` KQL function is a chart-managed Function CRD (`functions.items.auditEvents`). It is **disabled by default** because it depends on the `KubeAuditLogs` table, which is only populated after the audit pipeline infrastructure is provisioned (see WI-26 / [#194](https://github.com/Azure/taugrid/issues/194)). Once the EV2 audit pipeline step runs and data begins flowing, enable it in your values override:
> ```yaml
> functions:
>   items:
>     auditEvents:
>       enabled: true
>       database: Audit
> ```
> The ingestor registers a dedicated Audit DB kusto client (via `--logs-kusto-endpoints=Audit=...`), so the MI must have `Database Admin` on the `Audit` database — the same requirement as `Metrics` and `Logs`.

### Enabling in values.yaml

The audit database name is configured via `adx.auditDatabase` (default: `Audit`). Audit-related Functions query this database. No additional Helm values are needed for the pipeline infrastructure — that is managed by EV2 ([WI-26](https://github.com/Azure/taugrid/issues/194)). Enable `functions.items.auditEvents` once audit data is flowing.

## Troubleshooting

### Pods not starting

```bash
# Check events
kubectl describe pod -n adx-mon -l app.kubernetes.io/component=<component>

# Common issues:
# - Image pull errors: verify MCR connectivity
# - WAL permission denied: check supplementalGroups and storageDir permissions
# - CrashLoopBackOff: check logs for ADX connectivity or TLS errors
```

### No data in ADX

1. **Check ingestor logs** for connection errors:
   ```bash
   kubectl logs -n adx-mon -l app.kubernetes.io/component=ingestor --tail=50
   ```

2. **Verify ADX endpoint** is reachable from the cluster:
   ```bash
   kubectl exec -n adx-mon deploy/adx-mon-alerter -- \
     wget -qO- --spider https://mycluster.westus2.kusto.windows.net
   ```

3. **Check managed identity** — the MI must have `Database Admin` on each ADX database.

4. **Check leader election** — Functions, SummaryRules, and ManagementCommands require a leader. Verify the ingestor has the `app: ingestor` label:
   ```bash
   kubectl get pods -n adx-mon -l app=ingestor
   ```
   If no pods match, the leader election is broken. See [#315](https://github.com/Azure/taugrid/issues/315).

### Functions stuck in PermanentFailure

Functions fail if they reference tables that don't exist yet. Enable `managementCommands.precreateMetricsTables` and `managementCommands.precreateLogsTables` to pre-create all known tables before Functions are evaluated.

### Collector WAL permission errors

The collector writes WAL data to `storageDir` (default `/mnt/collector`). The init container sets permissions, but if it fails:
```bash
# Check init container logs
kubectl logs -n adx-mon <collector-pod> -c init-wal-dir
```

Ensure `podSecurityContext.supplementalGroups` includes GID `0` for log file access.

### High ADX table count (>500 tables)

Without `dropMetrics` filtering, every unique Prometheus metric creates a separate ADX table. Apply the `values-ai-runtime.yaml` preset or add your own `dropMetrics` patterns to `collector.prometheusScrape.dropMetrics`.

For emergency response when a misconfigured scrape target floods the ingestion queue, see the
[ADX Ingestion Queue Bombing Runbook](../../docs/runbooks/adx-ingestion-queue-bombing.md).

### Audit logs not appearing

See [Audit Log Pipeline — Critical Gotchas](#️-critical-gotchas) above. Most common issues:
1. AKS cluster is on Free tier (upgrade to Standard)
2. ADX data connection uses `JSON` instead of `MULTIJSON`
3. Event Hub is in a different region than AKS

## Uninstalling

```bash
helm uninstall adx-mon -n adx-mon
```

By default, CRDs are preserved on uninstall (`crds.keep: true`). To remove CRDs:
```bash
kubectl delete crd functions.adx-mon.azure.com alertrules.adx-mon.azure.com \
  summaryrules.adx-mon.azure.com managementcommands.adx-mon.azure.com \
  collectors.adx-mon.azure.com ingestors.adx-mon.azure.com \
  alerters.adx-mon.azure.com adxclusters.adx-mon.azure.com \
  metricsexporters.adx-mon.azure.com
```

## Links
- [ADX Ingestion Queue Bombing Runbook](../../docs/runbooks/adx-ingestion-queue-bombing.md)

- [adx-mon upstream](https://github.com/Azure/adx-mon)
- [Parent issue: Observability stack (#28)](https://github.com/Azure/taugrid/issues/28)
- [WI-26: EV2 Bicep for audit pipeline (#194)](https://github.com/Azure/taugrid/issues/194)
