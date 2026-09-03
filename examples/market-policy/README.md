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
- During a Tau-submitted run, the trainer also publishes immutable scalar
  history chunks for Stellar under
  `metrics-history-attempt-0/*.jsonl`.

The synthetic task exposes signal, volatility, inventory, episode phase,
phase sine/cosine, previous action, and micro-noise. The actor predicts Short,
Flat, or Long; the critic predicts the immediate synthetic reward. A held-out
quality gate requires at least 98% action accuracy and at most 0.04 reward RMSE.

## Train it on TauGrid

The checked-in configuration targets the `default` workspace and its writable
`workspace-data` PVC. The commands below use those values directly; a platform
with different workspace bindings should maintain a platform-specific copy of
the config rather than requiring researchers to edit commands while running
the example.

Render the workload before submitting it:

```bash
# from the repository root
make install-tau-cli
tau run --config examples/market-policy/tau.yaml --dry-run=client
```

The checked-in `metrics.offload` block pins the published TauGrid Portal
`0.4.0` image, which contains `taugrid-portal experiment offload metrics`, and
keeps the sidecar's checkpoint spool on the shared `/var/run/tau` emptyDir.
That avoids atomic-write incompatibilities on Azure File RWX volumes; an
abrupt pod loss can therefore lose rows that have not yet been remote-written.
Tau packages `train.py` into the submitted workload, so enabling Stellar does
not require a new training image build. The sidecar watches the JSONL chunks,
remote-writes the three scalar series (`train/loss`,
`validation/policy_accuracy`, and `validation/value_rmse`), and publishes a
terminal `tau/run_status` marker. Platform operators can still override the
checked-in image or spool directory with `TAU_METRICS_OFFLOAD_IMAGE` and
`TAU_METRICS_OFFLOAD_OUT`.

Run the exact trainer on an H200-capable TauGrid workspace:

```bash
tau run --config examples/market-policy/tau.yaml
tau run logs market-policy
```

The Ray driver requests a one-GPU remote task. Kueue admits the worker with the
portable `h200-141gb` GPU class, and the task fails if CUDA is unavailable. The
public TauGrid Ray/CUDA image is pinned by MCR digest, while `runtime.pip` pins
PyTorch and NumPy to the versions used by the trainer. The workspace therefore
needs package-index access while Ray prepares the runtime environment. On an
egress-restricted platform, bake those pinned packages into an approved
derivative image, update `runtime.image`, and remove `runtime.pip`.

The job mounts the configured workspace PVC and writes the browser artifact to
`/data/market-policy/tau-market-policy.json`. Fetch it after the RayCluster
releases its compute, supplying the same PVC name if the completed RayJob has
already been removed:

```bash
tau run get market-policy \
  --artifact tau-market-policy.json \
  --path /data/market-policy \
  --pvc workspace-data \
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

The target writes `.generated/tau-market-policy.cpu.json`; it does not replace
the checked-in H200 artifact. `make check` verifies that canonical artifact's
H200 provenance, model contract, quality gates, parameter count, and SHA-256.
The TauGrid manifest sets `MARKET_POLICY_USE_RAY=1` and
`MARKET_POLICY_REQUIRE_CUDA=1`, so submitted workloads run inside the Ray GPU
worker and fail rather than silently falling back to CPU.
