# TauGrid Development Guide

This guide describes the development workflow for TauGrid.

## Repository Layout

The public repository is organized by ownership and release boundary:

| Path | Contents |
| --- | --- |
| `cli/` | Go `tau` CLI, renderers, and lifecycle tools |
| `core/` | Shared library linked by the CLI and the portal |
| `portal/` | Stellar experiment tracking and the observability portal |
| `sdk/python/` | Python authoring SDK |
| `controllers/` | Workspace, cluster, and quota controllers |
| `monitoring/` | GPU health and metrics components |
| `charts/` | Public Helm charts |
| `images/` | Reproducible container image definitions |
| `cluster-overlays/` | Optional, cluster-specific Kubernetes configuration overlays for queues, storage, topology, and dashboards; not installed by the Helm charts |
| `examples/` | Runnable training, fine-tuning, inference, and local examples |
| `site/` | Hugo/Docsy documentation site |
| `tests/e2e/` | Portable cross-component and anonymous-install tests |

Live Microsoft environments, credentials, private registry configuration, EV2
assets, and cluster-specific GitOps state do not belong in this repository.

## Prerequisites

Install only the tools needed for the area you are changing:

- Git
- Go, using the version declared by the component's `go.mod`
- Python 3.10 or later and a virtual environment
- Helm 3
- Docker with BuildKit for image work
- `kubectl` and Kind for portable integration tests
- Hugo extended, Go, and Node.js for the Docsy site

Use versions pinned by `go.mod`, `pyproject.toml`, chart metadata, workflows, or
tooling files once they are available. Do not silently substitute a newer major
version in release or generated output.

## Get the Source

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid
git switch -c <type>/<short-description>
```

Do not commit local kubeconfigs, cloud credentials, `.env` files, generated
secrets, private endpoints, or test output containing sensitive information.

## Go Components

Run formatting and tests from each changed Go module:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
```

For CLI changes, verify command help and offline rendering where applicable:

```bash
go run ./cmd/tau --help
go run ./cmd/tau-gen --help
```

Controller changes should include unit tests for reconciliation behavior and a
Kind test when they affect CRDs, RBAC, admission, or generated Kubernetes
resources. Regenerate API artifacts with the component's checked-in Makefile;
never edit generated CRDs manually.

## Python SDK

Use an isolated environment:

```bash
cd sdk/python/python
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e '.[dev]'
python -m pytest
```

Keep the Python SDK's rendered intent compatible with the Go CLI. Changes to a
shared configuration contract require tests on both sides.

## Helm Charts

For every changed chart, run the chart's checked-in tests in addition to basic
Helm validation:

```bash
helm lint charts/<chart>
helm dependency build charts/<chart>
helm template test charts/<chart> --namespace taugrid-system >/dev/null
```

If the chart contains `helm-unittest` suites, run them with the repository's
pinned plugin or CI container. Cover default values and every public optional
feature affected by the change. Public defaults must use anonymously accessible
dependencies and immutable image versions or digests.

Merges to `main` that change `charts/**` run the Azure DevOps chart publishing pipeline in `.pipelines/publish-helm-charts.yml`. It packages all TauGrid-owned charts and publishes new versions under `oci://mcr.microsoft.com/aks/ai-runtime/helm`. Before each push, the pipeline checks the backing ACR that synchronizes to MCR. If the chart version already exists there, it skips that chart without comparing or replacing its package. A content-only change at the same version is still validated and packaged, but it is not republished. Bump `version` in the chart's `Chart.yaml` when a chart change should be published.

## Documentation Site

The site uses Hugo and Docsy. From `site/`, use its Makefile as the source of
truth:

```bash
make check
make build
make serve
```

Check public links, commands, image references, and navigation. Public docs must
not depend on Microsoft authentication, private repositories, internal DNS, or
unpublished artifacts.

## Containers

Container builds must be reproducible from a clean checkout and use public,
pinned base images. Build the changed image using its Makefile or documented
BuildKit command, then run its unit and smoke tests.

Do not publish from a contributor pull request. Release workflows own signing,
SBOM generation, vulnerability scanning, and promotion to MCR.

## Portable Integration Tests

Prefer offline rendering, unit tests, and Kind for pull-request validation.
Tests must not require an Azure subscription, a persistent Microsoft cluster,
private network access, or repository secrets unless they are explicitly marked
as maintainer-only post-merge validation.

Changes that affect Kubernetes APIs or installation should validate, as
applicable:

1. clean chart installation;
2. CRD and RBAC creation;
3. a bounded CPU-only smoke workload;
4. upgrade or migration behavior; and
5. cleanup without deleting user-owned resources.

GPU and AKS validation supplements portable tests; it does not replace them.

## Generated Files and Dependencies

- Commit generated files with the source change that produced them.
- Preserve upstream provenance for vendored or forked charts and code.
- Record third-party attribution required by the dependency's license.
- Use Dependabot or the component's documented update process for dependency
  changes.
- Avoid checking in build outputs, credentials, caches, or local environment
  state.

## Before Opening a Pull Request

- Run all component checks relevant to your change.
- Review the diff for secrets, private endpoints, internal identifiers, and
  unrelated changes.
- Update documentation and examples for public behavior changes.
- Explain compatibility and migration impact.
- Confirm new dependencies are publicly obtainable and license-compatible.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution and review process.
