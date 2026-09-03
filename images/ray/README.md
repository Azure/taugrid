# Ray Base Image

A multi-arch (amd64/arm64) container image based on [Azure Linux 3](https://github.com/microsoft/azurelinux) that bundles Python, [Ray](https://github.com/ray-project/ray) (with `default`, `data`, and `serve` extras), and the NVIDIA CUDA compiler toolkit. It serves as the base image for KubeRay workloads on AKS.

## What's Inside

| Component | Details |
|-----------|---------|
| **Base OS** | Azure Linux 3 (`mcr.microsoft.com/azurelinux/base/python`) |
| **Python** | 3.12 (configurable via `PYTHON_VERSION`) |
| **Ray** | 2.56.0 — `ray[default]`, `ray[data]`, `ray[serve]` |
| **protobuf** | Pinned `<5` — Ray Serve's `_proto_to_dict` reads `FieldDescriptor.label`, which protobuf 5+'s upb backend removed |
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
  { "python": "3.12", "ray": "2.56.0", "cuda": "13.0", "default": true }
]
```

Each entry defines a Python/Ray/CUDA combination to build. The entry with `"default": true` gets the `:latest` tag on canonical builds. There must be at most one default.

### Adding a new version combination

Add a new entry to `versions.json`:

```json
[
  { "python": "3.12", "ray": "2.54.0", "cuda": "13.0" },
  { "python": "3.12", "ray": "2.56.0", "cuda": "13.0", "default": true }
]
```

No workflow or Dockerfile changes are needed — the CI matrix picks it up automatically.

## Building

Version defaults are defined in the `Makefile` and can be overridden:

```bash
# Build single-arch image and load into local Docker
make docker-build

# Build with custom versions
make docker-build PYTHON_VERSION=3.12 RAY_VERSION=2.56.0 CUDA_VERSION=13.0

# Run smoke tests (verifies Python, Ray, and wget versions)
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

### Publish workflow (`.github/workflows/publish-ray-image.yaml`)

Builds and pushes multi-arch images to ACR using a matrix from `versions.json`.

| Trigger | Behavior |
|---------|----------|
| **Push to `main`** | Builds all combos from `versions.json`; updates `:latest` for the default combo |
| **`workflow_call`** | Builds exactly one combo (from caller inputs or Makefile defaults) with a PR-scoped tag that never overwrites `:latest` |
| **`workflow_dispatch`** | With version inputs → builds one combo; without → builds full matrix |

### Cleanup workflow (`.github/workflows/cleanup-ray-pr-tags.yaml`)

Prevents unbounded tag growth in ACR by removing PR-scoped image tags:

- On `pull_request:closed` — deletes tags for the specific PR.
- On a daily schedule (06:00 UTC) — prunes PR tags older than 7 days.

## Testing

`make test` runs the following smoke tests against a built image:

1. Python version matches `PYTHON_VERSION`
2. Ray version matches `RAY_VERSION`
3. `ray[default]` — `ray.dashboard` is importable
4. `ray[data]` — `ray.data` is importable
5. `ray[serve]` — `ray.serve` is importable
6. protobuf major version is `<5` and `FieldDescriptor.label` is present (Ray Serve proto compatibility)
7. GNU Wget 1.x is installed (not wget2)
8. RDMA userspace libraries (`ibverbs`, `rdmacm`, `mlx5`) are loadable
9. NCCL (`libnccl`) is loadable
