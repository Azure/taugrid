# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""RayJob entrypoint for COBOL continued pretraining.

TauGrid runs this file as the Ray driver on a CPU-only head pod, so it must not import
transformers, deepspeed or the training module. A Ray Train TorchTrainer starts 16
workers, one GPU each (2 worker pods x 8 GPUs), and each worker calls
continued_pretrain.main().

Ray Train initializes torch.distributed (NCCL on GPUs) and sets RANK, LOCAL_RANK,
WORLD_SIZE and MASTER_ADDR/PORT in every worker before train_fn runs, the same contract
torchrun gives. continued_pretrain is imported inside train_fn; TauGrid ships
this directory as the Ray working_dir, so the import resolves on the workers.
Configuration (MODEL_ID, OUTPUT_DIR, BATCH_SIZE, MAX_STEPS, ...) comes from the pod env,
which TauGrid sets on the head and on every worker.

HF Trainer keeps its own checkpointing and resume under OUTPUT_DIR; Ray Train only
stores its run metadata under OUTPUT_DIR/ray.
"""

import os

from ray.train import RunConfig, ScalingConfig
from ray.train.torch import TorchTrainer

NUM_WORKERS = 16
CPUS_PER_WORKER = 6


def train_fn():
    import continued_pretrain

    continued_pretrain.main()


def build_trainer(
    num_workers=NUM_WORKERS,
    use_gpu=True,
    cpus_per_worker=CPUS_PER_WORKER,
    storage_path=None,
):
    resources = {"CPU": cpus_per_worker}
    if use_gpu:
        resources["GPU"] = 1
    if storage_path is None:
        output_dir = os.environ.get(
            "OUTPUT_DIR", "/data/projects/taugrid-default/runs/cobol-cpt-grpo/cpt"
        )
        storage_path = os.path.join(output_dir, "ray")
    return TorchTrainer(
        train_loop_per_worker=train_fn,
        scaling_config=ScalingConfig(
            num_workers=num_workers, use_gpu=use_gpu, resources_per_worker=resources
        ),
        run_config=RunConfig(storage_path=storage_path),
    )


def main():
    build_trainer().fit()


if __name__ == "__main__":
    main()
