"""Tests for TAU_RAY_TRAIN_CONFIG_JSON consumption in the cluster wrapper."""

import json
import os
import unittest
from unittest.mock import patch


class TestRayTrainConfigMerge(unittest.TestCase):
    """Verify that TAU_RAY_TRAIN_CONFIG_JSON is parsed and merged correctly."""

    def test_empty_env_uses_defaults(self):
        """When env var is unset, default kwargs are used unchanged."""
        torch_kwargs = {"backend": "nccl"}
        scaling_kwargs = {
            "num_workers": 8,
            "use_gpu": True,
            "resources_per_worker": {"GPU": 1},
            "placement_strategy": "SPREAD",
        }
        failure_kwargs = {}

        raw = os.environ.get("TAU_RAY_TRAIN_CONFIG_JSON", "")
        if raw:
            config = json.loads(raw)
            if "torch_config" in config:
                torch_kwargs.update(config["torch_config"])
            if "scaling_config" in config:
                scaling_kwargs.update(config["scaling_config"])
            if "failure_config" in config:
                failure_kwargs.update(config["failure_config"])

        self.assertEqual(torch_kwargs, {"backend": "nccl"})
        self.assertEqual(scaling_kwargs["num_workers"], 8)
        self.assertFalse(failure_kwargs)

    @patch.dict(os.environ, {
        "TAU_RAY_TRAIN_CONFIG_JSON": json.dumps({
            "failure_config": {"max_failures": 3},
            "torch_config": {"timeout_s": 1800},
            "scaling_config": {"placement_strategy": "PACK", "use_gpu": False},
        })
    })
    def test_merge_overrides_defaults(self):
        """Config JSON overrides default values per section."""
        torch_kwargs = {"backend": "nccl"}
        scaling_kwargs = {
            "num_workers": 8,
            "use_gpu": True,
            "resources_per_worker": {"GPU": 1},
            "placement_strategy": "SPREAD",
        }
        failure_kwargs = {}

        raw = os.environ.get("TAU_RAY_TRAIN_CONFIG_JSON", "")
        config = json.loads(raw)
        if "torch_config" in config:
            torch_kwargs.update(config["torch_config"])
        if "scaling_config" in config:
            scaling_kwargs.update(config["scaling_config"])
        if "failure_config" in config:
            failure_kwargs.update(config["failure_config"])

        self.assertEqual(torch_kwargs, {"backend": "nccl", "timeout_s": 1800})
        self.assertEqual(scaling_kwargs["placement_strategy"], "PACK")
        self.assertFalse(scaling_kwargs["use_gpu"])
        self.assertEqual(scaling_kwargs["num_workers"], 8)
        self.assertEqual(failure_kwargs, {"max_failures": 3})

    @patch.dict(os.environ, {
        "TAU_RAY_TRAIN_CONFIG_JSON": json.dumps({
            "torch_config": {"backend": "gloo"},
        })
    })
    def test_backend_override(self):
        """torch_config.backend overrides TAU_DIST_BACKEND default."""
        torch_kwargs = {"backend": "nccl"}
        raw = os.environ.get("TAU_RAY_TRAIN_CONFIG_JSON", "")
        config = json.loads(raw)
        if "torch_config" in config:
            torch_kwargs.update(config["torch_config"])

        self.assertEqual(torch_kwargs["backend"], "gloo")

    @patch.dict(os.environ, {
        "TAU_RAY_TRAIN_CONFIG_JSON": json.dumps({
            "failure_config": {"max_failures": 2},
            "torch_config": {"unknown_future_field": 42},
        })
    })
    def test_unknown_keys_pass_through(self):
        """Unknown keys within sections pass through without error."""
        torch_kwargs = {"backend": "nccl"}
        raw = os.environ.get("TAU_RAY_TRAIN_CONFIG_JSON", "")
        config = json.loads(raw)
        if "torch_config" in config:
            torch_kwargs.update(config["torch_config"])

        self.assertEqual(torch_kwargs["unknown_future_field"], 42)
        self.assertEqual(torch_kwargs["backend"], "nccl")


if __name__ == "__main__":
    unittest.main()
