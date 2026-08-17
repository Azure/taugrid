# Tau examples

> **Status:** `operator runbook`
> **Intended use:** route users to the right Tau example by maturity, required
> setup, and recommended use.
> **Not for:** assuming every linked page is production-ready or appropriate for
> first-time onboarding.

Current Tau examples live in subdirectories with checked-in `tau.yaml` configs.
Prefer examples that keep each experiment variant in its own folder so the API
surface under test is easy to inspect. Legacy folders are labeled explicitly and
are retained only for historical references.

## Running a checked-in Tau manifest

Use `tau run --config` for config-first examples. Do not start new onboarding
from legacy `manifest.yaml` folders.

```bash
# from the repository root
make install-tau-cli
tau run --config examples/market-policy/tau.yaml --dry-run=client
tau run --config examples/market-policy/tau.yaml
```

For the supported repository-first GPU HPO path, follow the canonical
[GPU Ray Tune HPO on AKS walkthrough](../site/content/en/docs/examples/gpu-ray-tune.md).

## Status labels

Standalone Tau example pages should include the same status block near the top:

- `smoke/debug`: verifies a Tau path cheaply; not production data or scale.
- `operator runbook`: requires platform-owned setup, live cluster state, or
  manifests before a user can run it safely.
- `production-shaped`: close to the desired real workflow; use after smoke
  validation, not as first-time onboarding.
- `future UX`: product direction or design intent, not shipped behavior.
- `legacy`: retained for history or regression context; do not copy for new
  onboarding.

## Example index

| Example | Status | Intended use |
| --- | --- | --- |
| [`aks-cpu-quickstart`](./aks-cpu-quickstart/) | `operator runbook` | Creates a new CPU-only AKS cluster and runs `az aks create`, `tau cluster install`, a real CPU PyTorch RayJob, and live Stellar metrics. Creates billable Azure resources. |
| [`aks-gpu-quickstart`](./aks-gpu-quickstart/) | `operator runbook` | GPU counterpart to `aks-cpu-quickstart` on a single A100 node, including the AKS device plugin and MIG configuration issues. Verifies CUDA execution with evidence a CPU cannot produce. Creates expensive billable Azure resources. |
| [`market-policy`](./market-policy/) | `production-shaped` | Trains the exact compact actor-critic rendered in the documentation, requires CUDA on TauGrid, and exports browser-ready policy/value weights. |
| [`kind-smoke`](./kind-smoke/) | `smoke/debug` | Verify the local Tau -> Kueue -> Kubernetes Job path on a disposable Kind cluster before trying AKS/GPU-specific prepare and storage flows. |
| [`nanogpt-ray`](./nanogpt-ray/) | `smoke/debug` for local and small GPU runs; `production-shaped` for H200 target routes | Start with the 1-GPU smoke for onboarding, then use the pre-tokenized H200 configs for FineWeb target/regression work. |
| [`ray-tune-smoke`](./ray-tune-smoke/) | `smoke/debug` | Minimal Ray Tune HPO path: the researcher supplies `train_func(config)` and a search space, and Tau generates the Tuner/TorchTrainer wrapper. Use the [canonical AKS walkthrough](../site/content/en/docs/examples/gpu-ray-tune.md) for provider setup and the repository-first handoff. |
| FineWeb tokenization/data prep ([dataset contract](./nanogpt-ray/#dataset-contract), [registry runbook](../../docs/tau/dataset-catalog.md)) | `operator runbook` | Use pre-tokenized artifacts or the dataset registry handoff for target runs; inline tokenization is only a smoke/debug escape hatch. |
| Stellar grouping and wiki screenshots ([guide](../../docs/tau/stellar-ui.md#live-wiki-screenshot-refresh)) | `operator runbook` | Capture live NanoGPT/Stellar group pages from a real service for wiki assets; do not use mocks or future-only UI sketches. |
| [`cpu-multi-interest-ray`](./cpu-multi-interest-ray/) | `smoke/debug` | CPU Ray managed workflow demo using a checked-in `tau.yaml`; useful for validating config-first RayJob rendering without GPUs. |
| [`portal-ray-stellar`](./portal-ray-stellar/) | `operator runbook` | Single CPU RayJob that lights both portal detail-page links (Ray dashboard + Open in Experiments/Stellar); requires a pinned metrics-offload image and a namespace with a Ready TauWorkspace plus a writable `/data` PVC. |
| Serving compiled inference ([guide](../../docs/tau/tau-serve-compiled-inference-example.md)) | `future UX` | Review the intended `tau serve deploy` shape; do not present it as a completed quickstart. |
| Dataset catalog and ingest ([runbook](../../docs/tau/dataset-catalog.md)) | `operator runbook` | Seed, ingest, and verify dataset records before production-shaped FineWeb or RL data consumption. |
| Sandbox/RL cockpit scenario ([design](../../docs/design/agentic-experiment-tracking-design.md#56-executable-rl-experimenter-acceptance-scenario)) | `future UX` | Validate product direction for RL experiment loops; not a shipped dashboard workflow. |
