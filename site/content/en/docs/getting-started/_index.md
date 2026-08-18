---
title: Getting started
linkTitle: Getting started
weight: 2
description: Install Tau and complete a first workflow
---

AKS is TauGrid's first-class deployment target. The primary path has four
explicit phases:

1. **AKS setup (Azure/provider):** prepare the subscription, then provision or
   select AKS, its node pools, network access, managed Entra integration,
   provider add-ons, and Azure identities.
2. **TauGrid setup (Kubernetes layer):** install Kueue, KubeRay, and the Tau
   controllers and cluster-level policies into that reachable cluster.
3. **Workspace setup:** enable and validate the researcher namespace, queue,
   service account, storage contract, and repository connection handoff.
4. **Researcher workflow:** use the handed-off repository to submit and observe
   workloads.

Follow the first-class path in this order:

1. [Install Tau](install/).
2. Complete [AKS setup](aks-setup/).
3. Complete [TauGrid setup](taugrid-setup/) and its readiness gates.
4. [Connect to a platform workspace](workspace/).
5. Complete the [researcher quickstart](quickstart/).
6. For GPU HPO, continue with the canonical
   [Ray Tune on AKS walkthrough](../examples/gpu-ray-tune/).
