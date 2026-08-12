---
title: Portable core and provider integrations
weight: 4
description: What is Kubernetes-native and what belongs to the Azure reference
---

The portable Tau contract is Kubernetes-native:

- Repository configuration and validation.
- Job and RayJob intent.
- Kueue admission and KubeRay orchestration integration.
- Lifecycle commands, retry, resume, and local experiment evidence.
- Storage, identity, and telemetry interfaces.

The AKS reference adds concrete integrations:

- AKS and Microsoft Entra authentication.
- Azure workload identity and Key Vault.
- Azure Blob or Managed Lustre storage.
- Azure Container Registry and Microsoft Container Registry.
- adx-mon and ADX/Kusto scalar telemetry projections.

Provider integrations must stay behind explicit contracts. A project should not
need Azure-specific credentials or resource IDs in its ordinary `tau.yaml`.

See [Identity and security](../../concepts/identity/) and the
[Azure resource blueprint](https://github.com/Azure/taugrid/wiki/System-Architecture#tau-enabled-cluster-azure-resources).
