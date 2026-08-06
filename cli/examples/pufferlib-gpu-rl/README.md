# PufferLib GPU RL market making

> **Status:** `production-shaped`
> **Intended use:** evaluate whether Tau's config-first RayJob path is useful
> for GPU-backed reinforcement learning workloads that use PufferLib-style
> vectorized environments plus a CUDA policy/value learner.
> **Not for:** regulated trading, financial advice, or benchmarking PufferLib
> against its native Ocean environments.

This example trains a compact actor-critic policy for a synthetic market-making
task. PufferLib owns the vectorized environment interface; PyTorch trains the
policy and value head on the GPU; Tau owns the cluster contract:

- H200 scheduling through a checked-in `tau.yaml`,
- Kueue admission via the `dev` GPU LocalQueue,
- a RayJob with a CPU-only system head and one GPU execution worker,
- durable JSON/JSONL metrics under `/data/examples/pufferlib-gpu-rl/`.

The task is synthetic so it is safe to run repeatedly, but it is intentionally
not a hello-world: the default config drives more than 16 million vectorized
decisions, requires CUDA, writes a model checkpoint, and records throughput,
reward, action accuracy, entropy, and GPU metadata. The policy receives a small
oracle-action auxiliary loss so the run has an objective that should visibly
improve during a short cluster validation.

## Run

Start with a client dry-run:

```bash
# from the repository root
make install-tau-cli
tau run --config cli/examples/pufferlib-gpu-rl/tau.yaml --dry-run=client
```

Submit to the H200 pool:

```bash
tau run --config cli/examples/pufferlib-gpu-rl/tau.yaml
kubectl -n ray get rayjob pufferlib-gpu-rl
kubectl -n ray get pod -l tau.azure.com/job=pufferlib-gpu-rl -o wide
kubectl logs -n ray -l tau.azure.com/job=pufferlib-gpu-rl -f
```

For production, bake `pufferlib`, `gymnasium`, and any environment dependencies
into the image instead of relying on runtime `pip`. This example pins runtime
packages so the repository remains self-contained while the workload shape is
being evaluated, but image baking is the right next hardening step.
