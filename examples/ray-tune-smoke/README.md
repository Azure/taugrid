# Ray Tune Smoke Test

Minimal Ray Tune HPO smoke test for Tau. The researcher writes only
`train.py` (a `train_func(config)` function) and a `tau.yaml` with
the hyperparameter search space. Tau generates the Tuner + TorchTrainer
wrapper automatically.

## What the researcher writes

```python
# train.py
import ray.train

def train_func(config):
    lr = config["lr"]
    batch_size = config["batch_size"]
    for step in range(5):
        loss = compute_loss(lr, batch_size, step)
        ray.train.report({"loss": loss})
```

```yaml
# tau.yaml
execution:
  launcher: ray-tune
  metric: loss
  mode: min
  configs:
    lr: [0.001, 0.01, 0.1]
    batch_size: [32, 64]
```

## What Tau generates (invisible to the researcher)

A `_tau_tune_driver.py` that validates `train_func` on the head, reloads it
from Tau's staged source on each `TorchTrainer` worker, wraps it in
`TorchTrainer` → `Tuner`, forwards arbitrary worker metrics and checkpoints
with Ray's `TuneReportCallback`, reads the param space from env vars, and calls
`tuner.fit()`.

## Run

Place this example in a Tau-enabled research repository whose
`tau/workspace.connection.yaml` identifies a Ready workspace:

```text
research-repository/
  tau/
    workspace.connection.yaml
  tau.yaml
  train.py
```

From that repository, validate and submit without a context, namespace, or
queue override:

```bash
tau run --config tau.yaml --dry-run=client
tau run --config tau.yaml
```

Tau resolves the Kubernetes namespace and LocalQueue from the checked-in
workspace connection. The target keeps only workload-owned GPU intent
(`policy.gpu_class` and `compute.gpus_per_worker`); it does not hard-code
platform-owned placement.

The sweep is fully described by `tau.yaml` (`execution.launcher: ray-tune`
plus `execution.metric`, `execution.mode`, `execution.configs`,
`execution.num_samples`, `execution.max_concurrent_trials`). There is no
separate flat CLI path; edit the config and re-run.

The checked-in search space is three learning rates by two batch sizes, or six
trials. `max_concurrent_trials: 2` is an upper bound: on a one-GPU cluster the
trials execute sequentially because each `TorchTrainer` needs one GPU.

For provider setup, GPU readiness gates, workspace handoff, live-log capture,
success criteria, and cleanup, follow the canonical
[GPU Ray Tune HPO on AKS walkthrough](../../site/content/en/docs/examples/gpu-ray-tune.md).
