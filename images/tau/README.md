# Tau CLI image

This image packages the Tau Go CLI for cluster add-ons such as the
`run history record` lifecycle recorder. It follows the same build/publish
pattern as `images/gpu-metrics-collector`: multi-arch BuildX push to
`aksairuntime.azurecr.io/unlisted/aks/ai-runtime/tau:<short-sha>`, then a
workflow-created `:latest` tag. The image entrypoint is `tau`, so Kubernetes
manifests can pass normal Tau CLI arguments directly.

**Stellar and Portal are not in this image.** They moved to their own binary and
image, [`images/taugrid-portal`](../taugrid-portal/README.md). Anything that runs
`experiment ...` or `portal ...` — the always-on Stellar service, the Portal
Deployment, and the RayJob/RayJob-eval `metrics-offload` sidecar — must use the
`taugrid-portal` image. Pointing them at this image will fail with an unknown
command.

The runtime layer is distroless and intentionally shell-less. Consumers must not
render `/bin/sh -lc` for this image. For the metrics offload sidecar contract,
see
[`docs/tau/tau-metrics-offload-sidecar.md`](../../docs/tau/tau-metrics-offload-sidecar.md).

## Build

```bash
make test
make docker-build TAG="$(git rev-parse --short HEAD)"
```

The current pre-MCR image reference is:

```text
aksairuntime.azurecr.io/unlisted/aks/ai-runtime/tau:<short-sha>
```

`latest` is updated by the TauGrid core validation pipeline at
`the release pipeline` after the
short-SHA image has been pushed and verified.

## Pre-MCR readiness

Before MCR is enabled for Tau, keep doing the full staging path:

1. Build and push the multi-arch image to
   `aksairuntime.azurecr.io/unlisted/aks/ai-runtime/tau:<short-sha>`.
2. Verify the pushed short-SHA image with `docker buildx imagetools inspect` as
   part of `the release pipeline`.
3. Keep downstream manifests pinned to the staging ACR repository and immutable
   short-SHA tag.
4. Keep `:latest` as a workflow convenience tag only. Do not use it for rollout
   manifests or test-cluster installs.
5. Keep Helm defaults on the staging ACR repository until the MCR repository is
   registered through the `microsoft/mcr` repo, syndicated, and pull-tested.

## Promotion path

Tau is not published to MCR yet. Until that happens, downstream manifests should
pin a short-SHA tag from the TauGrid core chart test publish path:

```text
aksairuntime.azurecr.io/unlisted/aks/ai-runtime/tau:<short-sha>
```

The intended public MCR consumer path is:

```text
mcr.microsoft.com/aks/ai-runtime/tau:<same-short-sha>
```

The promotion should keep the tag immutable. Do not retag consumers to `latest`
for rollouts; use the same short-SHA tag that was built and verified by
`the release pipeline`.

MCR enablement and cutover steps:

1. Open and merge a `microsoft/mcr` PR that adds
   `teams/aks/ai-runtime/tau.yaml`, modeled after
   [`teams/aks/ai-runtime/gpu-metrics-collector.yaml`](https://github.com/microsoft/mcr/blob/main/teams/aks/ai-runtime/gpu-metrics-collector.yaml)
   from [microsoft/mcr#4921](https://github.com/microsoft/mcr/pull/4921).
2. Use the existing AKS image publishing pattern for gpu-metrics-collector:
   same subscription/resource group/registry/contact/security group fields, with
   repo name `unlisted/aks/ai-runtime/tau`. The `unlisted` repo name controls MAR
   discovery/onboarding; the external pull reference is still
   `mcr.microsoft.com/aks/ai-runtime/tau:<tag>`.
3. Confirm the promoted short-SHA tag can be pulled from
   `mcr.microsoft.com/aks/ai-runtime/tau`.
4. Keep the staging workflow publish path aligned with
   `aksairuntime.azurecr.io/unlisted/aks/ai-runtime/tau` unless the publishing
   registry itself changes.
5. Update downstream Helm values that consume the tau binary to
   `mcr.microsoft.com/aks/ai-runtime/tau` with the pinned short-SHA tag.
   `stellar.image` and `portal.image` are not among them — they track
   `taugrid-portal` and have their own promotion path.
