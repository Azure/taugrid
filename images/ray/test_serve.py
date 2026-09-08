# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise Serve's protobuf deserialization and local CPU-only startup."""

import subprocess
import sys
import textwrap
import unittest

from ray.serve._private.config import DeploymentConfig


class ServeProtoTest(unittest.TestCase):
    def test_deployment_config_round_trip(self):
        config = DeploymentConfig(
            num_replicas=2,
            user_config={"message": "protobuf smoke"},
            autoscaling_config={"min_replicas": 1, "max_replicas": 3},
            user_configured_option_names={
                "num_replicas",
                "user_config",
                "autoscaling_config",
            },
        )

        # This is the deserialization path used by the controller and replicas,
        # including nested messages, repeated fields, and pickled user config.
        restored = DeploymentConfig.from_proto_bytes(config.to_proto_bytes())

        self.assertEqual(restored.num_replicas, config.num_replicas)
        self.assertEqual(restored.user_config, config.user_config)
        self.assertEqual(restored.autoscaling_config.min_replicas, 1)
        self.assertEqual(restored.autoscaling_config.max_replicas, 3)
        self.assertEqual(
            restored.user_configured_option_names,
            config.user_configured_option_names,
        )

    def test_serve_startup(self):
        subprocess.run(
            [
                sys.executable,
                "-c",
                textwrap.dedent(
                    """
                    import json
                    from urllib.request import urlopen

                    import ray
                    from ray import serve

                    ray.init(
                        address="local",
                        num_cpus=2,
                        object_store_memory=100 * 1024 * 1024,
                        include_dashboard=True,
                        dashboard_host="127.0.0.1",
                    )
                    try:
                        @serve.deployment
                        def smoke():
                            return "serve-protobuf-ok"

                        handle = serve.run(smoke.bind(), name="protobuf-smoke")
                        assert handle.remote().result(timeout_s=30) == "serve-protobuf-ok"
                        with urlopen("http://127.0.0.1:8000/", timeout=30) as response:
                            assert response.read().decode() == "serve-protobuf-ok"
                        with urlopen(
                            "http://127.0.0.1:8265/api/serve/applications/", timeout=30
                        ) as response:
                            details = json.load(response)
                        assert details["applications"]["protobuf-smoke"]["status"] == "RUNNING"
                    finally:
                        serve.shutdown()
                        ray.shutdown()
                    """
                ),
            ],
            check=True,
            timeout=180,
        )


if __name__ == "__main__":
    unittest.main()
