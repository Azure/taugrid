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

A `_tau_tune_driver.py` that imports `train_func`, wraps it in
`TorchTrainer` → `Tuner`, reads the param space from env vars, and
calls `tuner.fit()`.

## Run

```bash
tau run --config tau.yaml --context <cluster>
```

The sweep is fully described by `tau.yaml` (`execution.launcher: ray-tune`
plus `execution.metric`, `execution.mode`, `execution.param_space`,
`execution.num_samples`, `execution.max_concurrent_trials`). There is no
separate flat CLI path; edit the config and re-run.
