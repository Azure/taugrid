# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""RayJob entrypoint for the vLLM eval harness.

TauGrid runs this file as the Ray driver on a CPU-only head pod, so it must not import
vllm or torch. The eval runs in one Ray task that reserves a whole GPU worker and calls
run_eval_vllm.main() there; cobc also runs inside that task, on the worker.

run_eval_vllm is imported inside the task because its module-level code imports vllm and
torch. TauGrid ships this directory as the Ray working_dir, so the import resolves on the
worker. Configuration (EVAL_DIR, MODEL_ID, OUT, MAX_NEW_TOKENS, TP_SIZE, ...) comes from
the pod env, which TauGrid sets on the head and on every worker.
"""

import os

import ray

NUM_GPUS = int(os.environ.get("TP_SIZE", "8"))


@ray.remote(num_gpus=NUM_GPUS, max_retries=0)
def run_eval():
    import run_eval_vllm

    run_eval_vllm.main()


def main():
    ray.init(address="auto", ignore_reinit_error=True)
    ray.get(run_eval.remote())


if __name__ == "__main__":
    main()
