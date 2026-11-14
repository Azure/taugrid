# Ray with preinstalled Torch

This optional variant preserves the existing Ray images. It derives from the
immutable Python 3.12 / Ray 2.58.0 / CUDA toolkit 13.0 multiarchitecture base
at `sha256:fc5f26891f1d9e170914f2052efbc49ceb9d50253acbb5955656455e120d8078`.
Torch `2.7.1+cu128` and its wheel-declared CUDA 12.8 dependencies are installed
globally. The default `nonroot:nonroot` user and global Python interpreter remain
unchanged. The Torch CUDA runtime version is distinct from the base toolkit.

Run Make from this directory. `PYTHON_VERSION=3.12`, `RAY_VERSION=2.58.0`,
and `CUDA_VERSION=13.0` are the only supported tuple; incompatible overrides fail
before a build or push. Tags use `-torch2.7.1-cu128`. The existing
`docker-push-arch` interface accepts `IMG`, `ARCH`, `SOURCE_REVISION`, and
`CACHE_REPO`; use a variant-specific cache and immutable image reference.
Publication remains governed separately.

The Docker build context is the repository root (`--file Dockerfile ../..`).
Image-owned scripts have one home, `scripts/images/ray-torch/`; source revision
and change tracking must include both that directory and `images/ray-torch/`.
The image exposes the package guard at `/opt/taugrid/ray-torch/check_image.py`.

## Checks

`make test-contract` is offline: package rejection tests, native build-interface
dry runs, and comparison of the real Serve pip specification with the image
smoke specification. The Go hardware-matrix contract separately checks the real
job pip specification.

`make test IMG=<image>` runs the existing base smoke tests and the variant checks.
Both the job runtime environment and a CPU Serve replica must use
system-site-packages virtualenvs and the same global Torch files, with networking
disabled and no copied Torch installation. Peak temporary allocation and the
final complete container writable layer must each stay below 512 MiB.
These image checks require an already built image; offline checks do not prove
their results. `make test-gpu IMG=<image>` additionally checks CUDA matrix
multiplication on every visible allocated GPU. Distributed numerical/rank,
placement, loss, and checkpoint coverage remain the hardware matrix's job.
