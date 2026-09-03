<p align="center">
  <img src="site/static/images/taugrid.png" alt="TauGrid logo" width="480">
</p>

<p align="center">
  Cloud-native AI infrastructure for GPU workloads on Kubernetes
</p>

[![Join us on Discord](https://img.shields.io/badge/Join%20us%20on-Discord-5865F2?logo=discord&logoColor=white)](https://discord.gg/nwbb6hBvds)

---

TauGrid runs GPU workloads on Kubernetes, including data preparation, distributed training, fine-tuning, and inference.

It combines the **tau CLI**, workload queueing and admission with **Kueue**, Ray cluster orchestration with **KubeRay**, node-level **GPU health monitoring**, and cluster and workload **observability**. Platform teams install this stack instead of assembling each component separately. Researchers use the CLI to submit and manage workloads without configuring Kubernetes directly.

## Features

| Capability | Description |
|---|---|
| **tau CLI** | Submit, monitor, and manage AI workloads from your terminal or CI pipeline |
| **Workload Queueing** | Fair-share scheduling, quota management, and priority-based admission via Kueue |
| **Ray Orchestration** | Managed Ray clusters for distributed training and inference via KubeRay |
| **GPU Health Monitoring** | Node-level diagnostics, automated drain on hardware faults, and fleet health visibility |
| **Observability** | Integrated metrics, logs, and dashboards for clusters, GPUs, and workloads |

## Architecture

TauGrid is built on open Kubernetes-native components:

```
┌─────────────────────────────────────────────────────────────────┐
│  Researcher                                                     │
│  tau run · status · logs · get · cancel                         │
└──────────────────────────┬──────────────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────────────┐
│  TauGrid Control Plane                                          │
│  ┌────────────┐  ┌──────────────┐  ┌────────────────────────┐  │
│  │   Kueue    │  │   KubeRay    │  │   GPU Health Monitor   │  │
│  │  Admission │  │ Orchestration│  │   Node Diagnostics     │  │
│  └────────────┘  └──────────────┘  └────────────────────────┘  │
└──────────────────────────┬──────────────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────────────┐
│  Compute, Data, and Evidence                                    │
│  CPU + GPU  ·  Datasets + Checkpoints  ·  Observability         │
└─────────────────────────────────────────────────────────────────┘
```

## Getting Started

### Prerequisites

- A Kubernetes cluster (1.30+) with GPU nodes
- `kubectl` configured for your cluster
- Helm 3.0 or later

TauGrid is built on Kubernetes and is tested end-to-end on AKS. Some
integrations, such as observability through Azure Data Explorer (Kusto),
remain Azure-specific. The project intends to support cloud and on-premises
Kubernetes environments without an Azure dependency; contributions toward
that goal are welcome.

### Install TauGrid

```bash
helm install taugrid \
  oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid \
  --version 0.4.0 \
  --namespace tau-system \
  --create-namespace
```

### Install the tau CLI

Install the latest stable GitHub Release with `curl`:

```bash
TAU_RELEASE_URL="$(
  curl -fsSL -o /dev/null -w '%{url_effective}' \
    https://github.com/Azure/taugrid/releases/latest
)"
TAU_VERSION="${TAU_RELEASE_URL##*/}"

curl -fsSL \
  "https://github.com/Azure/taugrid/releases/download/$TAU_VERSION/install.sh" |
  TAU_VERSION="$TAU_VERSION" bash

export PATH="$HOME/.local/bin:$PATH"
command -v tau
tau version --short
```

On Windows amd64, download and run the PowerShell installer:

```powershell
Invoke-WebRequest `
  https://github.com/Azure/taugrid/releases/latest/download/install.ps1 `
  -OutFile install.ps1
.\install.ps1

$env:PATH = "$env:LOCALAPPDATA\TauGrid\bin;$env:PATH"
tau version --short
```

The installer verifies the release checksum and version. It does not change the
user PATH; it prints the command for making that change in future terminals.

See the [installation guide](site/content/en/docs/platform-admin-guide/installation-guides/kubernetes.md#1-install-the-tau-cli)
for PATH persistence, pinned versions, the Python SDK wheel, upgrades, and the
advanced source installation.

The historical `v0.3.0` Release predates the release installer. If it is still
the latest Release, use the guide's source path until a newer Release is
available.

### Submit Your First Workload

Create a minimal `tau.yaml`:

```yaml
name: hello-gpu
image: mcr.microsoft.com/aks/ai-runtime/tau:0.4.0
command: ["python", "train.py"]
resources:
  gpu: 4
```

Then submit, check status, and stream logs:

```bash
tau run --config tau.yaml
tau run status hello-gpu
tau run logs hello-gpu
```

Browse the [runnable examples](examples/) for checked-in configurations and
step-by-step guides covering local, CPU, GPU, and Ray workloads.

## Container Images

TauGrid first-party images are published under the following MCR repositories:

| Component | Image repository |
|---|---|
| Tau | `mcr.microsoft.com/aks/ai-runtime/tau` |
| TauGrid Portal | `mcr.microsoft.com/aks/ai-runtime/taugrid-portal` |
| Tau core controller | `mcr.microsoft.com/aks/ai-runtime/tau-core-controller` |

Use the versioned tag or immutable digest documented by each release rather than a mutable `latest` tag.

## Helm Charts

TauGrid-owned charts are published as public OCI artifacts under
`oci://mcr.microsoft.com/aks/ai-runtime/helm`. Chart versions come from each
chart's `Chart.yaml`; published versions are immutable.

| Chart | OCI reference |
|---|---|
| TauGrid distribution | `oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid` |
| TauGrid core services | `oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid-core` |
| Tau core controller | `oci://mcr.microsoft.com/aks/ai-runtime/helm/tau-core-controller` |
| ADX monitoring | `oci://mcr.microsoft.com/aks/ai-runtime/helm/adx-mon` |

## Documentation

Full documentation is available at [https://azure.github.io/taugrid](https://azure.github.io/taugrid).

## Community

Join the [TauGrid Discord community](https://discord.gg/nwbb6hBvds) to meet
other users, discuss workloads, and get help.

## Roadmap

[ROADMAP.md](ROADMAP.md) describes where the project is going and what is
deliberately out of scope.

## Telemetry

TauGrid does not send telemetry to Microsoft by default. Platform operators can explicitly enable optional observability components and configure destinations such as Azure Data Explorer. To prevent remote telemetry export, keep those integrations disabled and do not configure a remote telemetry endpoint. Local Kubernetes logs, events, and metrics remain under the cluster operator's control.

## Third-Party Software

TauGrid integrates third-party open-source software including Kubernetes, Kueue, KubeRay, Ray, Node Problem Detector, and Prometheus components. These projects remain under their respective licenses. Exact dependency versions are recorded in the repository's module manifests, Helm chart metadata and lock files, and container build definitions.

## Contributing

This project welcomes contributions and suggestions. Please see [CONTRIBUTING.md](CONTRIBUTING.md) for details.

Most contributions require you to agree to a Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us the rights to use your contribution. For details, visit https://cla.opensource.microsoft.com.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Support

For bug reports and feature requests, please [open a GitHub issue](https://github.com/Azure/taugrid/issues).

## Security

Please see [SECURITY.md](SECURITY.md) for reporting security vulnerabilities.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft trademarks or logos is subject to and must follow [Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general). Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship. Any use of third-party trademarks or logos are subject to those third-party's policies.
