# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import ast
import contextlib
from dataclasses import dataclass
import io
import math
import os
from pathlib import Path
import unittest
from unittest.mock import Mock, patch


@dataclass
class TrainV2ScalingConfig:
    num_workers: int = 1
    use_gpu: bool = False
    trainer_resources: object = None

    def __post_init__(self):
        if self.trainer_resources is not None:
            raise DeprecationWarning("trainer_resources is V1-only")


class NanoGPTRuntimeTests(unittest.TestCase):
    def test_scaling_contract_rejects_v1_trainer_resources(self):
        for resources in ({}, {"CPU": 0}, {"CPU": 1}):
            with self.subTest(resources=resources):
                with self.assertRaises(DeprecationWarning):
                    TrainV2ScalingConfig(trainer_resources=resources)

    def test_main_constructs_train_v2_trainer_before_fit(self):
        tree = ast.parse(Path(__file__).with_name("nanogpt_ray_train.py").read_text())
        functions = [
            node for node in tree.body
            if isinstance(node, ast.FunctionDef)
            and node.name in {"main", "env_int", "final_metrics_from_result"}
        ]
        self.assertEqual(len(functions), 3)
        module = ast.Module(body=functions, type_ignores=[])
        for env, dimensions in [
            ({}, (4, 4, 256)),
            ({"NANOGPT_N_LAYER": "8", "NANOGPT_N_HEAD": "8", "NANOGPT_N_EMBD": "512"}, (8, 8, 512)),
        ]:
            with self.subTest(env=env):
                ray = Mock()
                ray.cluster_resources.return_value = {"GPU": 16}
                trainer = Mock()
                trainer.return_value.fit.return_value.metrics = {
                    "step": 2000, "world_size": 16, "loss": 1.0,
                }
                scaling = Mock(side_effect=TrainV2ScalingConfig)
                namespace = {
                    "os": os, "math": math, "ray": ray,
                    "ScalingConfig": scaling, "TorchTrainer": trainer,
                    "RunConfig": Mock(), "train_loop": Mock(),
                    "parse_dataset_contract": lambda: (["shard"], ["digest"], [100]),
                }
                exec(compile(module, "<nanoGPT runtime functions>", "exec"), namespace)
                output = io.StringIO()
                with patch.dict(os.environ, env, clear=True), contextlib.redirect_stdout(output):
                    namespace["main"]()
                scaling.assert_called_once_with(num_workers=16, use_gpu=True)
                trainer.assert_called_once()
                config = trainer.call_args.kwargs
                self.assertEqual(config["scaling_config"], TrainV2ScalingConfig(num_workers=16, use_gpu=True))
                self.assertEqual(
                    tuple(config["train_loop_config"][key] for key in ("n_layer", "n_head", "n_embd")),
                    dimensions,
                )
                trainer.return_value.fit.assert_called_once_with()
                self.assertIn("NANOGPT_RAY_TRAIN_SUCCESS", output.getvalue())


if __name__ == "__main__":
    unittest.main()
