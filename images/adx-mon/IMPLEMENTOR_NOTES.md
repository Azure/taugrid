# Implementor Notes — images/adx-mon

## WI-01: MSFT Go + CGO Validation

### Key Finding: Two MSFT Go Image Variants

The MCR registry has two relevant image families:

| Image | Base OS | Go version | GOEXPERIMENT default |
|-------|---------|-----------|---------------------|
| `mcr.microsoft.com/oss/go/microsoft/golang:1.25.8` | **Debian** (bookworm) | 1.25.8 | `systemcrypto` |
| `mcr.microsoft.com/oss/go/microsoft/golang:1.25.8-1-azurelinux3.0` | Azure Linux 3 | 1.25.0 (old, obsolete tag) | `nosystemcrypto` |

**Decision:** Use `1.25.8` (Debian). Reasons:
1. Correct Go version (adx-mon requires >= 1.25.3)
2. Debian base = `apt-get install libsystemd-dev` works (consistent with design doc §19.2.3)
3. The `-fips-` tags only go to 1.25.0-1; the standard `1.25.8` tag IS FIPS-capable via GOEXPERIMENT=systemcrypto

**Build deps on Debian builder:**
```dockerfile
apt-get install -y --no-install-recommends libsystemd-dev gcc
```
(NOT tdnf — that's AzureLinux only)

### WI-01 Test Results

**All 4 binaries build OK** with vendored deps + `GOEXPERIMENT=systemcrypto + CGO_ENABLED=1`.

**libsystemd linkage:** The collector uses `github.com/coreos/go-systemd/sdjournal` under build tag `linux && cgo`. With `CGO_ENABLED=1` and `libsystemd-dev` installed, the collector binary should dynamically link `libsystemd.so.0`. Confirmed via build log — the sdjournal CGO path is active.

**GOEXPERIMENT=systemcrypto in buildinfo:** Baked in when explicitly set at build time — `go tool buildinfo` will show it.

## WI-03: Dockerfile Decisions

### Collector (3-stage)
- Stage 1: MSFT Go Debian builder + `apt-get install libsystemd-dev gcc`
- Stage 2: Azure Linux `tdnf install systemd-libs` → provides `libsystemd.so.0` + 6 transitive deps
- Stage 3: Azure Linux distroless (no shell, minimal attack surface)
- Runtime systemd-libs copied from AzureLinux (same pattern as upstream Dockerfile.collector)

### Ingestor/Alerter/Operator (2-stage)
- Stage 1: MSFT Go Debian builder + `apt-get install gcc`
- Stage 2: Azure Linux distroless
- No systemd dependency

### Version arg: `ADX_MON_REF` not `ADX_MON_VERSION`
Adx-mon has **no published tags** as of 2026-04-06. Default is `main`. The design doc refers to `v0.2.0` as a future target. Using `ADX_MON_REF` (not `VERSION`) to make it clear this accepts branch names, tags, or SHAs.

## WI-04: GHA Workflow

Follows `publish-gpu-metrics-collector.yaml` exactly:
- `workflow_dispatch` with `releaseTag` input
- 1ES pool: `1es-<runner-pool>`
- `az login --identity` → `az acr login`
- `make push` with REGISTRY/TAG/ADX_MON_REF from input
- Tags `:latest` via `az acr import`
- Verifies each image in ACR post-push

## WI-02: MCR Onboarding PR

PR: https://github.com/microsoft/mcr/pull/4930

File: `teams/aks/ai-runtime/adx-mon.yaml`

Same subscriptionId/resourceGroup/registry/securityGroup as `gpu-monitoring.yaml`. Registers 4 repos:
- `public/aks/ai-runtime/adx-mon/collector`
- `public/aks/ai-runtime/adx-mon/ingestor`
- `public/aks/ai-runtime/adx-mon/alerter`
- `public/aks/ai-runtime/adx-mon/operator`

**Note:** Sovereign cloud syndication not configured in MCR YAML — handled at ACR level by `AKSMCRImagesCommon` registry configuration per design §19.2.7.
