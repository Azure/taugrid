# ADX monitor images

TauGrid builds the upstream [Azure/adx-mon](https://github.com/Azure/adx-mon)
components with Microsoft Go and `GOEXPERIMENT=systemcrypto`. The collector
image supports both `linux/amd64` and `linux/arm64`; the other component images
remain `linux/amd64`.

Build a collector image for the developer machine's native platform:

```bash
cd images/adx-mon
make docker-build-collector ADX_MON_VERSION=main TAG=dev
docker run --rm mcr.microsoft.com/aks/ai-runtime/adx-mon/collector:dev --help
```

Build and push one multi-platform collector manifest to an authorized backing
registry:

```bash
cd images/adx-mon
make docker-push-collector \
  ADX_MON_VERSION=main \
  TAG=<tag> \
  PUBLISH_REGISTRY=<backing-registry>/public/aks/ai-runtime/adx-mon
```

This target builds `linux/amd64,linux/arm64` by default. Override
`COLLECTOR_PLATFORMS` only for development validation. Do not overwrite an
existing release tag.

Inspect the published manifest and confirm both platforms are present:

```bash
docker buildx imagetools inspect \
  <backing-registry>/public/aks/ai-runtime/adx-mon/collector:<tag>
```

Build each platform without publishing:

```bash
cd images/adx-mon
docker buildx build --platform linux/amd64 -f Dockerfile.collector .
docker buildx build --platform linux/arm64 -f Dockerfile.collector .
```
