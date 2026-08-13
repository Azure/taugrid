# TauGrid market policy training

> **Status:** `production-shaped`
> **Intended use:** train the exact compact actor-critic model rendered in the
> TauGrid documentation and export browser-ready weights.
> **Not for:** live trading, regulated financial workloads, or financial advice.

This example owns both sides of the demonstration:

- `train.py` trains an 8-input, 24-hidden-unit actor-critic with 316 parameters.
- `tau.yaml` creates a RayJob whose driver dispatches that trainer to one
  TauGrid GPU worker.
- The trainer writes `tau-market-policy.json`, the exact format loaded by the
  documentation site's module Worker.

The synthetic task exposes signal, volatility, inventory, episode phase,
phase sine/cosine, previous action, and micro-noise. The actor predicts Short,
Flat, or Long; the critic predicts the immediate synthetic reward. A held-out
quality gate requires at least 98% action accuracy and at most 0.04 reward RMSE.

## Train it on TauGrid

Render the workload before submitting it:

```bash
# from the repository root
make install-tau-cli
tau run --config cli/examples/market-policy/tau.yaml --dry-run=client
```

Run the exact trainer on the `aks-ai-runtime-flex` H200 pool:

```bash
tau run --config cli/examples/market-policy/tau.yaml \
  --context aks-ai-runtime-flex-admin \
  --namespace tau-default
tau run logs market-policy \
  --context aks-ai-runtime-flex-admin \
  --namespace tau-default
```

The Ray driver requests a one-GPU remote task. Kueue admits the worker with the
`nd-h200-v5` resource flavor, and the task fails if CUDA is unavailable. The
image is pinned by digest with Ray, PyTorch, and CUDA preinstalled because Flex
workers do not have PyPI egress.

The job mounts the `blob-training` workspace PVC and writes the browser artifact
to `/data/tau-workspaces/default/market-policy/tau-market-policy.json`. Fetch it
after the RayCluster releases its compute:

```bash
tau run get market-policy \
  --artifact tau-market-policy.json \
  --context aks-ai-runtime-flex-admin \
  --namespace tau-default \
  --path /data/tau-workspaces/default/market-policy \
  --pvc blob-training \
  --output raw
```

The final log line reports the device, H200 model, Ray version, quality metrics,
parameter count, and durable output path.

## Verify a deterministic CPU export

The site target invokes the same `train.py` entrypoint with CPU explicitly
selected and trains twice to verify byte-for-byte deterministic output:

```bash
cd site
make train-market-policy
```

The target writes `.hugo_cache/tau-market-policy.cpu.json`; it does not replace
the checked-in H200 artifact. `make check` verifies that canonical artifact's
H200 provenance, model contract, quality gates, parameter count, and SHA-256.
The TauGrid manifest sets `MARKET_POLICY_USE_RAY=1` and
`MARKET_POLICY_REQUIRE_CUDA=1`, so submitted workloads run inside the Ray GPU
worker and fail rather than silently falling back to CPU.
