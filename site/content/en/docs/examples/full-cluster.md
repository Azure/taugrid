---
title: Provision a Kueue and Ray AKS baseline
linkTitle: Full AKS baseline
weight: 30
description: Build a modular AKS, Kueue, Ray, storage, and workload baseline before adding Tau policy.
---

{{< maturity status="experimental" reviewed="2026-07-16" >}}

The `kueue-and-ray-on-aks` example is a three-module platform tutorial:

1. provision AKS, storage, Kueue, KubeRay, and optional GPU capacity;
2. configure queue quota and borrowing; and
3. submit training, batch inference, and serving workloads.

It is useful for learning the infrastructure Tau expects. It does **not**
provision a TauWorkspace or replace the platform-owned Tau enablement steps.

## Prerequisites

- an Azure subscription that passes the
  [subscription readiness gate](../../getting-started/azure-subscription/) and
  an approved Terraform identity;
- Azure CLI, Terraform, kubectl, Helm, and local Python dependencies;
- permission to provision AKS, networking, storage, and identities; and
- for the default GPU path, at least 96 vCPUs of
  `Standard NDASv4_A100 Family` quota in the target region.

Use the CPU-only option when GPU quota is unavailable.

## Provision module 1

```bash
git clone https://github.com/Azure/taugrid-examples.git
cd taugrid-examples/kueue-and-ray-on-aks/1-infrastructure/terraform

terraform init
terraform apply -var="subscription_id=<your-subscription-id>"
```

For a CPU-only cluster:

```bash
terraform apply \
  -var="subscription_id=<your-subscription-id>" \
  -var="gpu_enabled=false"
```

Continue through
[Module 2: Kueue queues](https://github.com/Azure/taugrid-examples/tree/main/kueue-and-ray-on-aks/2-kueue-queues)
and
[Module 3: workloads](https://github.com/Azure/taugrid-examples/tree/main/kueue-and-ray-on-aks/3-workloads)
only after Terraform completes and cluster credentials are working.

## Identity and cost boundaries

Organization Conditional Access can prevent local Azure CLI Microsoft Graph
token acquisition. Use an approved service principal, workload identity, OIDC,
or managed-identity automation context rather than exporting tokens or
introducing account keys.

The GPU default provisions an eight-A100 node and can incur substantial cost.
Destroy the tutorial infrastructure when finished:

```bash
terraform destroy -var="subscription_id=<your-subscription-id>"
```

Before adding Tau, review the
[provider boundary](../../overview/provider-boundaries/) and
[platform workspace enablement](../../tasks/platform/enable-workspace/).
