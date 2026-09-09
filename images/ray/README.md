# Ray Base Image

A multi-arch (amd64/arm64) container image based on [Azure Linux 3](https://github.com/microsoft/azurelinux) that bundles Python, [Ray](https://github.com/ray-project/ray) (with `default`, `data`, and `serve` extras), and the NVIDIA CUDA compiler toolkit. It serves as the base image for KubeRay workloads on AKS.

## What's Inside

| Component | Details |
|-----------|---------|
| **Base OS** | Azure Linux 3 (`mcr.microsoft.com/azurelinux/base/python`) |
| **Python** | 3.12 (configurable via `PYTHON_VERSION`) |
| **Ray** | 2.56.1 build target — `ray[default]`, `ray[data]`, `ray[serve]` |
| **protobuf** | Resolved by Ray's dependencies without an image-specific upper bound; Ray 2.56.1 includes the upstream protobuf 7 compatibility fix |
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
├── versions.json    # Version matrix: Python × Ray × CUDA combos to build
└── README.md
```

## Version Matrix

Version combinations are defined in [`versions.json`](versions.json):

```json
[
  { "python": "3.12", "ray": "2.54.0", "cuda": "13.0" },
  { "python": "3.12", "ray": "2.55.1", "cuda": "13.0" },
  { "python": "3.12", "ray": "2.56.0", "cuda": "13.0" },
  { "python": "3.12", "ray": "2.56.1", "cuda": "13.0", "default": true }
]
```

Each entry records a Python/Ray/CUDA combination intended for release. There
must be at most one `"default": true` entry. The Makefile uses its own defaults;
the matrix is release input, not evidence of published availability. Older
entries remain for historical build combinations; they do not inherit Ray
2.56.1's Serve fix and must pass their own dependency and behavioral checks.

### Adding a new version combination

Add the combination to `versions.json`, preserving older entries. When changing
the default, move `"default": true` to the new entry and update `RAY_VERSION`
in the Makefile. Pass each non-default combination explicitly to the Makefile's
build and test targets, and coordinate matrix consumption with the ADO release
owner.

## Building

Version defaults are defined in the `Makefile` and can be overridden:

```bash
# Build single-arch image and load into local Docker
make docker-build

# Build with custom versions
make docker-build PYTHON_VERSION=3.12 RAY_VERSION=2.56.1 CUDA_VERSION=13.0

# Run image smoke tests
make test

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
make docker-push-arch ARCH=amd64 IMG=<registry>/ray:py3.12-ray2.56.1-cuda13.0

# On an arm64 runner: build + push the arm64 image natively
make docker-push-arch ARCH=arm64 IMG=<registry>/ray:py3.12-ray2.56.1-cuda13.0

# On any runner, after both of the above succeed: combine into one multi-arch
# manifest at the canonical tag (pure registry metadata op — no build)
make docker-push-manifest IMG=<registry>/ray:py3.12-ray2.56.1-cuda13.0
```

`docker-push-arch` pushes to `$(IMG)-$(ARCH)` (e.g. `...:py3.12-ray2.56.1-cuda13.0-amd64`);
`docker-push-manifest` reads those two arch-suffixed tags and publishes the
combined manifest list at `$(IMG)` itself.

### Makefile Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PYTHON_VERSION` | `3.12` | Python version for the base image |
| `RAY_VERSION` | `2.56.1` | Ray version to install via pip |
| `CUDA_VERSION` | `13.0` | CUDA toolkit version (converted to dash form for NVIDIA RPM packages internally) |
| `IMG` | `mcr.microsoft.com/aks/ai-runtime/ray:<tag>` | Fully-qualified destination tag used by `docker-build`, `test`, `clean`, `docker-push-arch`, and `docker-push-manifest` |
| `ACR_REGISTRY` | *(required for `docker-push`)* | Backing ACR hostname for producer-side multi-arch pushes; consumers use MCR |
| `PLATFORMS` | `linux/amd64,linux/arm64` | Architectures for the `docker-push` multi-arch build |
| `ARCH` | *(empty)* | Single architecture (`amd64` or `arm64`) for `docker-push-arch` |
| `CACHE_REPO` | *(empty)* | Set to enable registry-based BuildKit cache |

### Image Tag Format

The local producer tag includes the source SHA. A stable public consumer tag
omits that suffix. The currently published 2.56.0 tag is:

```
mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0
```

The 2.56.1 build target in this source is not a claim that a corresponding
consumer image is published. CLI defaults and chart image allowlists remain on
the published version until the ADO release and coordinated consumer rollout.

## CI/CD

Ray CI and publication are owned by a separate Azure DevOps (ADO) repository,
not GitHub workflows in this checkout. Green GitHub PR checks are not evidence
that this image has been rebuilt or published. ADO release owners must build
and test both architectures and publish through the approved ACR-to-MCR process;
contributor PRs must not publish images.

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
6. `pip check` succeeds
7. GNU Wget 1.x is installed (not wget2)
8. RDMA userspace libraries (`ibverbs`, `rdmacm`, `mlx5`) are loadable
9. NCCL (`libnccl`) is loadable

These smoke tests do not exercise Serve deployment or request handling.
Release validation must separately cover Serve startup and RayService rollout;
imports alone do not cover the protobuf deserialization path.

Python protobuf 7.34 removed `FieldDescriptor.label` (see the
[protobuf migration guide](https://protobuf.dev/support/migration/#fielddescriptorlabel)).
[Ray 2.56.1](https://github.com/ray-project/ray/releases/tag/ray-2.56.1) fixes
Serve's handling upstream by using `is_repeated` with a legacy fallback
([upstream fix](https://github.com/ray-project/ray/pull/64592)). This image uses
that patch release rather than a protobuf upper-bound workaround or a local
Ray monkeypatch. The regression combination is Ray 2.56.1 with protobuf 7.36.0;
verify the resolved versions and Serve behavior during release validation.
