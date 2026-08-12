// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

const tuneDriverScript = `import importlib, importlib.util, json, os, sys

module_name = os.environ["TAU_TUNE_TRAIN_MODULE"]
module_path = os.environ.get("TAU_TUNE_TRAIN_PATH")


def load_train_func():
    if module_path:
        spec = importlib.util.spec_from_file_location(module_name, module_path)
        if spec is None or spec.loader is None:
            raise ImportError(f"cannot load Tau Tune training module from {module_path}")
        mod = importlib.util.module_from_spec(spec)
        sys.modules[module_name] = mod
        spec.loader.exec_module(mod)
    else:
        mod = importlib.import_module(module_name)
    train_func = getattr(mod, "train_func")
    if not callable(train_func):
        raise TypeError(f"{module_name}.train_func must be callable")
    return train_func


# Preserve the previous fail-fast behavior: syntax/import/contract errors are
# reported by the driver before Tune allocates trial resources.
load_train_func()

import ray
from ray import tune
from ray.train.torch import TorchTrainer, TorchConfig
from ray.train import ScalingConfig, RunConfig
from ray.tune.integration.ray_train import TuneReportCallback

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


def train_loop(config):
    return load_train_func()(config)


def train_driver(config):
    trainer = TorchTrainer(
        train_loop,
        train_loop_config=config,
        torch_config=TorchConfig(backend=backend),
        scaling_config=ScalingConfig(num_workers=num_workers, use_gpu=use_gpu),
        run_config=RunConfig(callbacks=[TuneReportCallback()]),
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
