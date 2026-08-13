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

Set `storage.data_pvc` in `tau.yaml` to the writable PVC supplied for your
workspace. Tau resolves the target namespace and LocalQueue from the `default`
workspace; change `policy.workspace` if your platform handoff uses another
name.

Render the workload before submitting it:

```bash
# from the repository root
make install-tau-cli
tau run --config cli/examples/market-policy/tau.yaml --dry-run=client
```

Run the exact trainer on an H200-capable TauGrid workspace:

```bash
tau run --config cli/examples/market-policy/tau.yaml
tau run logs market-policy
```

The Ray driver requests a one-GPU remote task. Kueue admits the worker with the
portable `h200-141gb` GPU class, and the task fails if CUDA is unavailable. The
image is pinned by digest with Ray, PyTorch, and CUDA preinstalled, so the
workload does not depend on runtime package downloads.

The job mounts the configured workspace PVC and writes the browser artifact to
`/data/market-policy/tau-market-policy.json`. Fetch it after the RayCluster
releases its compute, supplying the same PVC name if the completed RayJob has
already been removed:

```bash
tau run get market-policy \
  --artifact tau-market-policy.json \
  --path /data/market-policy \
  --pvc <workspace-pvc> \
  --output raw
```

The final log line reports the device, GPU model, Ray version, quality metrics,
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
