# Tau CLI image

This image packages the Tau Go CLI for cluster add-ons such as the
`run history record` lifecycle recorder. It follows the same build/publish
pattern as `images/gpu-metrics-collector`: a multi-arch BuildX push to the
backing ACR, followed by MCR syndication. The image entrypoint is `tau`, so
Kubernetes manifests can pass normal Tau CLI arguments directly.

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

The public consumer reference is:

```text
mcr.microsoft.com/aks/ai-runtime/tau:<taugrid-core-chart-version>
```

Release tags match the `taugrid-core` chart version. `latest` and short-SHA
tags remain build/promotion conveniences and are not chart defaults.

## Publish and consume

1. Build and push the multi-arch image to the backing repository configured by
   the local Makefile.
2. Verify the source image with `docker buildx imagetools inspect` in
   `the release pipeline`.
3. Publish the release tag matching the `taugrid-core` chart version.
4. Confirm that version tag is available from MCR before publishing or using
   the matching chart release. Consumers that require immutable references may
   override the chart default with the release image digest.
5. Keep `:latest` as a workflow convenience tag only; do not use it for rollout
   manifests or test-cluster installs.

`stellar.image` and `portal.image` do not consume this image. They use
`mcr.microsoft.com/aks/ai-runtime/taugrid-portal`.
