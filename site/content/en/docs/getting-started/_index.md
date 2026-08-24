---
title: Getting started
linkTitle: Getting started
weight: 2
description: Install the Tau CLI, prepare AKS, install TauGrid, and complete a first workflow
---

AKS is TauGrid's first-class deployment target. The primary path has five explicit phases:

1. **Install Tau CLI (local prerequisite):** install the released `tau` CLI on each platform-operator or researcher workstation that will use it.
2. **AKS setup (Azure/provider):** prepare the subscription, then provision or select AKS, its node pools, network access, managed Entra integration, provider add-ons, and Azure identities.
3. **TauGrid setup (Kubernetes layer):** use the Tau CLI to install the released MCR Helm chart, then verify the in-cluster control plane.
4. **Workspace setup:** enable and validate the researcher namespace, queue, service account, storage contract, and repository connection handoff.
5. **Researcher quickstart:** use the handed-off repository to submit and observe workloads.

Follow the first-class path in this order:

1. [Install Tau CLI](install/).
2. Complete [AKS setup](aks-setup/).
3. Complete [TauGrid setup](taugrid-setup/) and verify the installation.
4. Complete [Tau Workspace Setup](tau-workspace-setup/).
5. Complete the [researcher quickstart](quickstart/).
6. For GPU HPO, continue with the canonical [Ray Tune on AKS walkthrough](../examples/gpu-ray-tune/).

For a self-service AKS evaluation environment, follow [Provision the platform
baseline](../examples/full-cluster/). It provisions AKS, installs TauGrid,
creates a workspace, and runs a smoke workload. Continue with the researcher
quickstart after that baseline succeeds.
