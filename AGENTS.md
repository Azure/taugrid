# AGENTS.md

This file provides guidance to AI coding agents when working with code in this repository.

## What This Is

TauGrid is a cloud-native AI infrastructure platform for GPU workloads on Kubernetes. It provides the `tau` CLI, workload queueing (Kueue), Ray cluster orchestration (KubeRay), GPU health monitoring, and observability. The repo is organized as a multi-module Go monorepo with a Python SDK.

## Build and Test Commands

The top-level Makefile provides repository-wide validation, installation, and documentation entry points:

```bash
make build                   # build first-party Go components
make test                    # run Go, offline E2E, and Python tests
make lint                    # lint Go and Python source
make check                   # build, test, lint, and check license headers
make install-tau             # install the CLI and optional Python SDK
make install-tau-cli         # install the CLI only
make install-tau-sdk         # install the Python SDK in the active Python
make install-taugrid-portal  # install the Portal CLI
make tau-docs-check          # build and validate the documentation site
```

Each component can also be built and tested from its own directory:

### CLI (`cli/`)
```bash
cd cli
make build        # produces bin/tau and bin/tau-gen
make test         # go test ./...
make lint         # go vet + gofmt + staticcheck v0.7.0
make test-kind-e2e      # Kind-based smoke test
make test-integration   # integration tests (requires built binary, 15m timeout)
```

### Core library (`core/`)
```bash
cd core
go test ./...
go vet ./...
```

### Portal (`portal/`)
```bash
cd portal
make build    # produces bin/taugrid-portal
make test
make lint     # go vet + gofmt + staticcheck v0.7.0
```

### Tau core controller (`controllers/tau-core/`)
```bash
cd controllers/tau-core
make build        # produces bin/tau-core-controller
make test         # go test -race -count=1 ./...
make lint         # go vet + gofmt
make generate     # regenerate API deepcopy
make manifests    # regenerate CRDs (controller-gen v0.20.0)
make test-kind-e2e  # Kind-based integration test
```

### GPU monitoring (`monitoring/gpu-health-checker/`, `monitoring/gpu-metrics-collector/`)
```bash
cd monitoring/gpu-health-checker
make test     # CGO_ENABLED=1 go test ./...
make lint     # golangci-lint run ./...
```

### Python SDK (`sdk/python/python/`)
```bash
cd sdk/python/python
python3 -m venv .venv && source .venv/bin/activate
pip install -e '.[dev]'
python -m pytest          # run tests
python -m ruff check .    # lint (ruff 0.16.1)
```

### Portable end-to-end module (`tests/e2e/`)
```bash
cd tests/e2e
AI_RUNTIME_E2E=0 go test -count=1 ./...  # offline tests; never contacts a cluster
go vet ./...
```

The top-level `make check` always forces `AI_RUNTIME_E2E=0`. Run real-cluster
tests only when explicitly requested and only after verifying the kubeconfig:

```bash
cd tests/e2e
AI_RUNTIME_E2E=1 go test -v -timeout 15m ./...
```

### Helm Charts (`charts/`)
```bash
helm lint charts/<chart>
helm dependency build charts/<chart>
helm template test charts/<chart> --namespace taugrid-system >/dev/null

# Unit tests require the helm-unittest plugin (not built into Helm):
helm plugin install https://github.com/helm-unittest/helm-unittest.git --version v1.1.1  # Helm 3
helm plugin install oci://ghcr.io/helm-unittest/helm-unittest/unittest:1.1.1 --verify=false  # Helm 4
helm unittest charts/<chart>  # if tests/ exist
```

### Documentation site (`site/`)
```bash
cd site
make check    # builds Hugo site + runs link/content/accessibility checks
make serve    # local dev server with drafts
```

## Running a Single Test

```bash
# Go — run one test by name in any module
cd cli && go test -run TestFoo ./internal/jobrender/...
cd controllers/tau-core && go test -race -run TestReconcile ./internal/controller/...

# Python SDK
cd sdk/python/python && python -m pytest tests/test_foo.py -k test_bar
```

## CI Validation

CI runs on every PR to main (`.github/workflows/taugrid-validation.yml`). It builds, formats, vets, lints, and tests each Go module independently. The Python SDK has its own workflow triggered on `sdk/python/**` changes. Helm charts validate on `charts/**` changes against Helm 3 and 4.

Key CI settings: `GOFLAGS=-mod=readonly`, `GOTOOLCHAIN=local`. Tests run with `-count=1` (no caching). The controller module uses `-race`.

## TauGrid Release Version Bump

TauGrid uses one release version for the `taugrid`, `taugrid-core`, and
`tau-core-controller` charts plus the `tau`, `taugrid-portal`, and
`tau-core-controller` images. Helm and MCR use `X.Y.Z`; the Git tag uses
`vX.Y.Z`. The tag publishes the `tau` and `tau-gen` CLI binaries plus the
independently versioned Python SDK wheel; `gpu-monitoring` and `adx-mon` keep
their own release versions.

1. Update `version`/`appVersion` in the three `Chart.yaml` files and the two
   first-party dependency versions in `charts/taugrid/Chart.yaml`.
2. Update their image tags, the controller Kustomize image, the CLI default
   chart version, install examples, and version assertions.
3. Run `scripts/ci/vendor-taugrid-dependencies.sh charts/taugrid`, Helm lint and
   unit tests for the three charts, `cd cli && go test ./internal/cli`, and
   `python3 scripts/check-license-headers.py`.
4. Before tagging, add `cli/releases/vX.Y.Z.md`. After merge, push the annotated
   tag; the TauGrid release starts automatically and publishes the CLI binaries
   plus the independently versioned Python SDK wheel. Retry it by dispatching
   the workflow from the same tag ref with the same tag input.

## Architecture

### Module dependency graph

The `core/` module is the shared library. Both `cli/` and `portal/` depend on it via a `replace` directive pointing at `../core`. The controller and monitoring modules are independent Go modules.

### CLI (`cli/`)

Two binaries: `tau` (user-facing CLI built with cobra) and `tau-gen` (code generation tool). Internal packages under `cli/internal/` handle discrete concerns:
- `jobrender` / `rayjobrender` — render Kubernetes Job and RayJob manifests
- `clusteraccess` — AKS credential acquisition
- `dataset` / `datasetingest` — dataset management and ingestion
- `workspace` / `workspaceconnection` — workspace lifecycle
- `queueresolve` / `queuequota` — Kueue queue selection and quota display
- `storage` / `storageprobe` — blob storage operations
- `resume` — checkpoint-based job resumption

### Core (`core/`)

Shared domain types and clients:
- `runconfig` — run configuration schema (shared contract between Python SDK and Go CLI)
- `kube` — Kubernetes client helpers
- `kueueapi` — Kueue API interactions
- `experiment` / `expkusto` / `exptelemetry` — experiment tracking and telemetry
- `topology` — GPU/node topology
- `queue` / `resourceprofile` — queue definitions and resource profiles
- `version` — build version injection (used by ldflags)

### Controller (`controllers/tau-core/`)

Standard Kubernetes controller pattern:
- `api/v1alpha1/` — CRD types (generates deepcopy and CRD manifests)
- `internal/controller/` — reconciliation logic
- `config/crd/` — generated CRD YAML

### Portal (`portal/`)

Experiment tracking and observability portal:
- `internal/expstore` — experiment data persistence
- `internal/portal` / `portalapi` — web UI serving and API
- `internal/autocapture` / `expcapture` — automatic metric capture

### Python SDK (`sdk/python/python/`)

Package name: `tau`. Researchers write Python; the Go CLI remains the Kubernetes executor. The SDK's run configuration must stay compatible with `core/runconfig`.

## Conventions

- Go toolchain: 1.26.6 (see `.go-version`); modules require Go 1.26.5
- Linting: staticcheck v0.7.0 for cli/core/portal; golangci-lint for monitoring modules
- `staticcheck.conf` at repo root disables specific style checks (ST1000, ST1003, ST1005, ST1016, ST1020-ST1023, S1016) — do not re-enable without fixing all findings
- Python: requires 3.10+, lint with ruff
- Never edit generated CRDs manually — use `make manifests` in the controller
- AI agents must run `python3 scripts/check-license-headers.py` before committing source changes. Use `./scripts/add-license-headers.py` only for Microsoft-authored files, preserve all existing copyright notices, and never add a Microsoft copyright notice to third-party code.
- Container images go through MCR; never publish from a contributor PR
- Integration tests must not require Azure subscriptions or private network access unless explicitly marked maintainer-only
