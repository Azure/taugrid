# TauGrid Portal image

This image packages the `taugrid-portal` Go CLI, which serves the Stellar
experiment dashboard and the unified observability Portal. It was split out of
`images/tau` so the tau CLI does not have to carry the entire web surface, its
embedded JS/CSS/HTML assets, and the experiment store.

It follows the same build/publish pattern as [`images/tau`](../tau/README.md):
multi-arch BuildX push to
`aksairuntime.azurecr.io/unlisted/aks/ai-runtime/taugrid-portal:<short-sha>`,
then MCR syndication and a workflow-created `:latest` tag. The entrypoint is
`taugrid-portal`, so Kubernetes manifests pass normal CLI arguments directly.

## Who needs this image

Anything that runs `experiment ...` or `portal ...`:

| Consumer | Command |
| --- | --- |
| Stellar service (`taugrid-core` chart, `stellar.image`) | `experiment init`, `experiment serve` |
| Metrics importer sidecar (`taugrid-core` chart) | `experiment offload metrics --watch` |
| Portal Deployment (`taugrid-core` chart, `portal.image`) | `portal serve` |
| RayJob/RayJob-eval metrics offload sidecar | `experiment offload metrics --watch` |

The `tau` image still owns `run history record` (the lifecycle recorder) and
every other tau verb. The two images are not interchangeable in either
direction.

## Build

```bash
make test
make docker-build TAG="$(git rev-parse --short HEAD)"
```

The build context is the repository root, because the module depends on
`core` through a `replace` directive:

```bash
docker buildx build -f images/taugrid-portal/Dockerfile .
```

The runtime layer is distroless and intentionally shell-less. Consumers must not
render `/bin/sh -lc` for this image.

## Publish and consume

1. Build and push the multi-arch image to
   `aksairuntime.azurecr.io/unlisted/aks/ai-runtime/taugrid-portal:<short-sha>`.
2. Verify the pushed short-SHA image with `docker buildx imagetools inspect` as
   part of `the release pipeline`.
3. Confirm the same short-SHA tag is available from
   `mcr.microsoft.com/aks/ai-runtime/taugrid-portal:<short-sha>`.
4. Pin downstream manifests to that MCR short-SHA tag or its immutable digest.
5. Keep `:latest` as a workflow convenience tag only. Do not use it for rollout
   manifests or test-cluster installs.

## Migration note

The live TauGrid Portal overlays pin a published `taugrid-portal` digest. Keep
that immutable digest contract when promoting a new build through
`the release pipeline`; do not fall back to the pre-split `tau`
image or a mutable tag.
