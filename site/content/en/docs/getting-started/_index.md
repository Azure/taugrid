---
title: Getting started
linkTitle: Getting started
weight: 2
description: Install Tau and complete a first workflow
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

AKS is TauGrid's first-class deployment target. The primary path has three
explicit phases:

1. **AKS setup (Azure/provider):** prepare the subscription, then provision or
   select AKS, its node pools, network access, managed Entra integration,
   provider add-ons, and Azure identities.
2. **Kubernetes/TauGrid setup:** install Kueue, KubeRay, and the Tau
   controllers and policies into that reachable cluster, then enable a
   workspace.
3. **Researcher workflow:** use the handed-off repository to submit and observe
   workloads.

Follow the first-class path in this order:

1. [Install Tau](install/).
2. [Prepare an Azure subscription and AKS](azure-subscription/).
3. Check the [AKS versus Kubernetes/TauGrid boundary and readiness
   gate](prerequisites/).
4. [Connect to a platform workspace](workspace/).
5. Complete the [researcher quickstart](quickstart/).
6. For GPU HPO, continue with the canonical
   [Ray Tune on AKS walkthrough](../examples/gpu-ray-tune/).

[Local and Kind evaluation](kind/) verifies Tau's portable Kubernetes behavior
when Azure is not required. It is not a substitute for validating AKS identity,
networking, GPU, storage, or quota integrations.
