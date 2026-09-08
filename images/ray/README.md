# Ray Base Image

A multi-arch (amd64/arm64) container image based on [Azure Linux 3](https://github.com/microsoft/azurelinux) that bundles Python, [Ray](https://github.com/ray-project/ray) (with `default`, `data`, and `serve` extras), and the NVIDIA CUDA compiler toolkit. It serves as the base image for KubeRay workloads on AKS.

## What's Inside

| Component | Details |
|-----------|---------|
| **Base OS** | Azure Linux 3 (`mcr.microsoft.com/azurelinux/base/python`) |
| **Python** | 3.12 (configurable via `PYTHON_VERSION`) |
| **Ray** | 2.56.0 — `ray[default]`, `ray[data]`, `ray[serve]` |
| **protobuf** | Constrained `<7` — Ray 2.56 Serve reads `FieldDescriptor.label`, removed in Python protobuf 7.34; protobuf 6.x remains compatible |
| **CUDA toolkit** | nvcc, ptxas, nvrtc, nvvm/libdevice, libcurand-devel (from NVIDIA RHEL 9 repos) |
| **NCCL** | NVIDIA Collective Communications Library — multi-GPU all-reduce, broadcast; uses RDMA/IB transport when available |
| **RDMA userspace** | rdma-core, libibverbs, librdmacm — enables NCCL InfiniBand transport on IB-capable nodes (e.g. H200/NDR) |
| **C/C++ toolchain** | gcc, g++, ninja-build, python3-devel — needed by flashinfer JIT and vLLM extensions at runtime |
| **GNU Wget 1.x** | Built from source to replace Azure Linux 3's wget2, which breaks KubeRay exec-based health probes on dual-stack pods |

The image runs as the `nonroot` user and exposes Ray's default ports:

| Port | Service |
|------|---------|
| 6379 | GCS server |
| 8265 | Dashboard |
| 10001 | Client |

## Project Structure

```
images/ray/
├── Dockerfile       # Multi-stage build: GNU Wget builder → final Ray image
├── Makefile         # Build, test, and push targets
├── test_serve.py     # CPU-only Serve proto and startup smoke tests
├── versions.json    # Version matrix: Python × Ray × CUDA combos to build
└── README.md
```

## Version Matrix

Version combinations are defined in [`versions.json`](versions.json):

```json
[
  { "python": "3.12", "ray": "2.56.0", "cuda": "13.0", "default": true }
]
```

Each entry records a Python/Ray/CUDA combination intended for release. There
must be at most one `"default": true` entry. The Makefile uses its own defaults;
this file does not currently trigger automated builds or `:latest` publication.

### Adding a new version combination

Add a new entry to `versions.json`:

```json
[
  { "python": "3.12", "ray": "2.54.0", "cuda": "13.0" },
  { "python": "3.12", "ray": "2.56.0", "cuda": "13.0", "default": true }
]
```

Pass each new combination explicitly to the Makefile's build and test targets.
There is currently no Ray CI matrix consuming this file automatically.

## Building

Version defaults are defined in the `Makefile` and can be overridden:

```bash
# Build single-arch image and load into local Docker
make docker-build

# Build with custom versions
make docker-build PYTHON_VERSION=3.12 RAY_VERSION=2.56.0 CUDA_VERSION=13.0

# Run smoke tests, including a local CPU-only Serve deployment
make test

# Run only dependency consistency and Serve behavior checks
make test-serve

# Build multi-arch manifest and push to registry (emulates the non-native
# platform with QEMU under the hood — see the native split below for CI)
make docker-push
```

### Native per-architecture builds (no QEMU)

`docker-push` builds both platforms in a single `buildx build --platform
linux/amd64,linux/arm64` invocation, which relies on QEMU emulation for
whichever architecture isn't native to the host running the command. For CI
running each architecture on its own native runner in parallel, use
`docker-push-arch` + `docker-push-manifest` instead — no emulation, faster
builds:

```bash
# On an amd64 runner: build + push the amd64 image natively
make docker-push-arch ARCH=amd64 IMG=<registry>/ray:py3.12-ray2.56.0-cuda13.0

# On an arm64 runner: build + push the arm64 image natively
make docker-push-arch ARCH=arm64 IMG=<registry>/ray:py3.12-ray2.56.0-cuda13.0

# On any runner, after both of the above succeed: combine into one multi-arch
# manifest at the canonical tag (pure registry metadata op — no build)
make docker-push-manifest IMG=<registry>/ray:py3.12-ray2.56.0-cuda13.0
```

`docker-push-arch` pushes to `$(IMG)-$(ARCH)` (e.g. `...:py3.12-ray2.56.0-cuda13.0-amd64`);
`docker-push-manifest` reads those two arch-suffixed tags and publishes the
combined manifest list at `$(IMG)` itself.

### Makefile Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PYTHON_VERSION` | `3.12` | Python version for the base image |
| `RAY_VERSION` | `2.56.0` | Ray version to install via pip |
| `CUDA_VERSION` | `13.0` | CUDA toolkit version (converted to dash form for NVIDIA RPM packages internally) |
| `IMG` | `mcr.microsoft.com/aks/ai-runtime/ray:<tag>` | Fully-qualified destination tag used by `docker-build`, `test`, `clean`, `docker-push-arch`, and `docker-push-manifest` |
| `ACR_REGISTRY` | *(required for `docker-push`)* | Backing ACR hostname for producer-side multi-arch pushes; consumers use MCR |
| `PLATFORMS` | `linux/amd64,linux/arm64` | Architectures for the `docker-push` multi-arch build |
| `ARCH` | *(empty)* | Single architecture (`amd64` or `arm64`) for `docker-push-arch` |
| `CACHE_REPO` | *(empty)* | Set to enable registry-based BuildKit cache |

### Image Tag Format

The local producer tag includes the source SHA. The corresponding stable public
consumer tag omits that suffix, for example:

```
mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0
```

## CI/CD

This checkout does not contain a Ray image publishing or PR-tag cleanup
workflow. The general image-validation workflow does not build Ray images,
so green PR checks are not evidence that this image has been rebuilt.
Release owners must build and test both architectures and publish through the
approved ACR-to-MCR process; contributor PRs must not publish images.

A source fix alone does not repair an already-published image. Issue #206
requires publication of the corrected image and a RayService rollout check
against its digest before the runtime incident can be considered resolved.

## Testing

`make test` runs the following smoke tests against a built image:

1. Python version matches `PYTHON_VERSION`
2. Ray version matches `RAY_VERSION`
3. `ray[default]` — `ray.dashboard` is importable
4. `ray[data]` — `ray.data` is importable
5. `ray[serve]` — `ray.serve` is importable
6. `pip check` succeeds and Serve deployment config round-trips through protobuf (including nested/repeated fields and user config)
7. A CPU-only Serve replica starts and answers both a deployment-handle request and HTTP; the dashboard reports its application as `RUNNING`
8. GNU Wget 1.x is installed (not wget2)
9. RDMA userspace libraries (`ibverbs`, `rdmacm`, `mlx5`) are loadable
10. NCCL (`libnccl`) is loadable

`make test-serve` uses Python's standard-library `unittest` runner and starts
an isolated Ray instance inside the container, with a 180-second startup/request
test timeout. It does not connect to Kubernetes or require a GPU. The proto
round-trip reproduces the `FieldDescriptor.label` error with protobuf 7.36.0;
checking imports or descriptor attributes alone does not cover this path.

The compatibility boundary is Python protobuf 7.34, not 5.x (see the
[protobuf migration guide](https://protobuf.dev/support/migration/#fielddescriptorlabel)).
Resolving all three Ray 2.56 extras with `<7` allows protobuf 6.x and current
OpenTelemetry proto packages rather than forcing the older versions needed
by `<5`. Revisit the constraint when upgrading Ray, using the behavioral
smoke tests rather than assuming a newer Ray version has removed every use.
