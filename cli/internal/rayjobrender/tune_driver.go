// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

const tuneDriverScript = `import json, os, sys

sys.path.insert(0, "/script")

module_name = os.environ["TAU_TUNE_TRAIN_MODULE"]
mod = __import__(module_name)
train_func = getattr(mod, "train_func")

import ray
from ray import tune
from ray.train.torch import TorchTrainer, TorchConfig
from ray.train import ScalingConfig, RunConfig

param_space_raw = json.loads(os.environ["TAU_TUNE_PARAM_SPACE"])
param_space = {}
for k, v in param_space_raw.items():
    if isinstance(v, list):
        param_space[k] = tune.grid_search(v)
    else:
        param_space[k] = v

num_workers = int(os.environ.get("TAU_NUM_WORKERS", "1"))
use_gpu = os.environ.get("TAU_DIST_BACKEND", "gloo") == "nccl"
backend = os.environ.get("TAU_DIST_BACKEND", "gloo")
metric = os.environ["TAU_TUNE_METRIC"]
mode = os.environ.get("TAU_TUNE_MODE", "min")
num_samples = int(os.environ.get("TAU_TUNE_NUM_SAMPLES", "1"))
max_concurrent = int(os.environ.get("TAU_TUNE_MAX_CONCURRENT_TRIALS", "1"))


def train_driver(config):
    trainer = TorchTrainer(
        train_func,
        train_loop_config=config,
        torch_config=TorchConfig(backend=backend),
        scaling_config=ScalingConfig(num_workers=num_workers, use_gpu=use_gpu),
        run_config=RunConfig(),
    )
    trainer.fit()


if not ray.is_initialized():
    ray.init()

tuner = tune.Tuner(
    train_driver,
    param_space=param_space,
    tune_config=tune.TuneConfig(
        metric=metric,
        mode=mode,
        num_samples=num_samples,
        max_concurrent_trials=max_concurrent,
    ),
)
results = tuner.fit()
best = results.get_best_result()
print(f"Best config: {best.config}")
print(f"Best {metric}: {best.metrics[metric]}")
`

const tuneDriverFilename = "_tau_tune_driver.py"
