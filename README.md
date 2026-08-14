<p align="center">
  <img src="site/static/images/taugrid.png" alt="TauGrid logo" width="480">
</p>

<p align="center">
  Cloud-native AI infrastructure for GPU workloads on Kubernetes
</p>

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

- A Kubernetes cluster (1.28+) with GPU nodes
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
  --version 0.2.3 \
  --namespace tau-system \
  --create-namespace
```

### Install the tau CLI

Until a GitHub Release is published, install the CLI from a source checkout:

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid
make install-tau-cli

TAU_BIN_DIR="$(go env GOBIN)"
test -n "$TAU_BIN_DIR" || TAU_BIN_DIR="$(go env GOPATH)/bin"
export PATH="$TAU_BIN_DIR:$PATH"

command -v tau
tau version --short
```

Use the Go version declared in `cli/go.mod`. The Make target installs `tau` and
`tau-gen` into `GOBIN`, or `GOPATH/bin` when `GOBIN` is unset, and warns when
that directory is not on `PATH`. See the
[installation guide](site/content/en/docs/getting-started/install.md) for PATH
persistence, upgrades, the optional Python SDK, and the verified-binary path
available after a GitHub Release is published.

### Submit Your First Workload

```bash
# Submit a training job
tau run --queue default --gpu 4 -- python train.py

# Check status
tau status

# Stream logs
tau logs <job-name>
```

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
