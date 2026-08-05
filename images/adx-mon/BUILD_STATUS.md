# AdxMon Image Pipeline — Build Status

**Last updated:** 2026-04-06  
**Branch:** `agent/implementor/main`  
**Implementor:** Implementor agent

---

## WI Summary

| WI | Title | Status | Notes |
|----|-------|--------|-------|
| WI-01 | Validate MSFT Go + CGO | ✅ DONE | All 4 binaries build; dlopen libsystemd confirmed; GOEXPERIMENT=systemcrypto baked in |
| WI-02 | MCR onboarding PR | ✅ SUBMITTED / ⏳ PENDING REVIEW | [microsoft/mcr#4930](https://github.com/microsoft/mcr/pull/4930) — awaiting MCR team review |
| WI-03 | Dockerfiles (4x) | ✅ DONE | `images/adx-mon/Dockerfile.{collector,ingestor,alerter,operator}` committed |
| WI-04 | GHA publish workflow | ✅ DONE | `.github/workflows/publish-adx-mon-images.yaml` committed |
| WI-05 | First build + MCR verify | ⏳ BLOCKED | Blocked on WI-02 MCR PR merge; then trigger workflow with `ADX_MON_VERSION=main` |
| WI-06 | Helm chart — CRDs | 🕒 TODO | — |
| WI-07 | Helm chart — helpers + values | 🕒 TODO | — |
| WI-08 | Helm chart — Operator | 🕒 TODO | — |
| WI-09 | Helm chart — Ingestor | 🕒 TODO | — |
| WI-10 | Helm chart — Collector DaemonSet | 🕒 TODO | — |
| WI-11 | Helm chart — Collector Singleton | 🕒 TODO | — |
| WI-12 | Helm chart — Alerter | 🕒 TODO | — |
| WI-13 | Helm chart — KSM subchart | 🕒 TODO | — |
| WI-14 | Switch GHCR → MCR images | ⏳ BLOCKED | Blocked on WI-05 |
| WI-15 | Function CRDs (KQL views) | 🕒 TODO | — |
| WI-16 | AlertRule CRDs | 🕒 TODO | — |
| WI-17 | Grafana dashboards | 🕒 TODO | — |
| WI-18 | values-ai-runtime.yaml preset | 🕒 TODO | — |
| WI-19 | ArgoCD application + Kustomize | 🕒 TODO | — |
| WI-20 | Cluster overlay examples | 🕒 TODO | — |
| WI-21 | SummaryRule CRDs (cost) | 🕒 TODO | — |
| WI-22 | GPU rate ConfigMap | 🕒 TODO | — |
| WI-23 | ManagementCommand CRDs | 🕒 TODO | — |
| WI-24 | Helm chart README + NOTES.txt | 🕒 TODO | — |
| WI-25 | End-to-end validation | 🕒 TODO | — |

---

## WI-01 Detail: MSFT Go + CGO Validation

**Result: PASS**

| Check | Result |
|-------|--------|
| Builder image | `mcr.microsoft.com/oss/go/microsoft/golang:1.25.8` (Debian/bookworm) |
| Go version | go1.25.8 linux/amd64 |
| `libsystemd-dev` available | ✅ `apt-get install libsystemd-dev` provides `/usr/lib/x86_64-linux-gnu/libsystemd.so` |
| `pkg-config --libs libsystemd` | ✅ returns `-lsystemd` |
| collector build | ✅ `collector: OK` |
| ingestor build | ✅ `ingestor: OK` |
| alerter build | ✅ `alerter: OK` |
| operator build | ✅ `operator: OK` |
| All 4 start | ✅ all respond to `--help` |
| libsystemd linkage | ✅ via **dlopen** at runtime (not static link — `go-systemd/sdjournal` uses `dlopen("libsystemd.so.0", RTLD_LAZY)`) |
| `GOEXPERIMENT=systemcrypto` in buildinfo | ✅ (Debian image default GOEXPERIMENT is empty — explicit flag bakes it in) |

**Why NOT `-azurelinux3.0` image:**  
The `1.25.8-1-azurelinux3.0` image has `GOEXPERIMENT=nosystemcrypto` baked in as its default. The Debian `1.25.8` image has an empty default, so `GOEXPERIMENT=systemcrypto` takes effect cleanly.

**No tags on adx-mon upstream:**  
Adx-mon has zero published tags. `ADX_MON_VERSION=main` is the only option until the AdxMon team cuts a release. Dockerfiles and Makefile default to `main`; GHA workflow takes `releaseTag` input.

---

## WI-02 Detail: MCR Onboarding PR

**PR:** https://github.com/microsoft/mcr/pull/4930  
**File:** `teams/aks/ai-runtime/adx-mon.yaml`  
**Status:** Submitted, awaiting `validate-mcr-yml` CI + MCR team review  
**Format:** Matches PR #4912 template (same team, same infra)

Registered repos:
- `public/aks/ai-runtime/adx-mon/collector`
- `public/aks/ai-runtime/adx-mon/ingestor`
- `public/aks/ai-runtime/adx-mon/alerter`
- `public/aks/ai-runtime/adx-mon/operator`

---

## WI-03 Detail: Dockerfiles

All 4 in `images/adx-mon/`:

| File | Stages | Notes |
|------|--------|-------|
| `Dockerfile.collector` | 3: builder → libsystemdsource → distroless | systemd-libs copied from AzureLinux stage |
| `Dockerfile.ingestor` | 2: builder → distroless | No systemd dep |
| `Dockerfile.alerter` | 2: builder → distroless | No systemd dep |
| `Dockerfile.operator` | 2: builder → distroless | No systemd dep |

All accept `ADX_MON_VERSION` build arg (default: `main`).

---

## WI-04 Detail: GHA Workflow

File: `.github/workflows/publish-adx-mon-images.yaml`  
Pattern: follows `publish-gpu-metrics-collector.yaml` exactly  
Pool: `1ES.Pool=1es-<runner-pool>`  
Trigger: `workflow_dispatch` with `releaseTag` input  
ACR: `aksmcrimagescommon.azurecr.io/public/aks/ai-runtime/adx-mon/{collector,ingestor,alerter,operator}`

---

## Next Steps

1. **WI-02**: Wait for MCR PR #4930 to merge
2. **WI-05**: Trigger `publish-adx-mon-images.yaml` once MCR merge is confirmed  
   → `gh workflow run publish-adx-mon-images.yaml -f releaseTag=main`
3. **WI-06**: Start Helm chart — CRDs (no blockers)
4. **WI-07**: Helm chart helpers + values.yaml (in parallel with WI-06)
