# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase


class GpuRuntimeTests(ScriptTestCase):
    def test_throttle_reasons(self):
        versions = {"GPU_DRIVER_VERSIONS": '("580.126.09")'}
        output = self.run_script(
            "check_gpu_throttle.sh",
            expected=1,
            env={
                **versions,
                "NVIDIA_SMI_THROTTLE_OUTPUT": "0x0000000000000008",
            },
        )
        self.assertIn("GPU 0 throttled", output)

        for allowed_reason in (
            "0x0000000000000004",
            "0x0000000000000001",
        ):
            with self.subTest(reason=allowed_reason):
                self.run_script(
                    "check_gpu_throttle.sh",
                    env={
                        **versions,
                        "NVIDIA_SMI_THROTTLE_OUTPUT": allowed_reason,
                    },
                )

    def test_throttle_query_failure_is_not_silenced(self):
        output = self.run_script(
            "check_gpu_throttle.sh",
            expected=1,
            env={
                "GPU_DRIVER_VERSIONS": '("")',
                "NVIDIA_SMI_THROTTLE_FAIL": "1",
            },
        )
        self.assertIn("return code is 1", output)
        healthy = self.run_script(
            "check_gpu_throttle.sh", env={"GPU_DRIVER_VERSIONS": '("")'}
        )
        self.assertIn("No GPU throttling detected", healthy)

    def test_dcgm_required_and_skipped_paths(self):
        self.run_script(
            "check-dcgm-health.sh",
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_HEALTH_OUTPUT": "DCGM health check passed",
            },
        )
        output = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_HEALTH_OUTPUT": "DCGM diagnostic failure",
                "DCGMI_HEALTH_EXIT_CODE": "3",
            },
        )
        self.assertIn("DCGM diagnostic failure", output)
        self.assertIn("return code 3", output)

        skipped = self.run_script(
            "check-dcgm-health.sh", env={"NPD_DCGM_REQUIRED": "0"}
        )
        self.assertIn("not required for this profile", skipped)

    def test_dcgm_wrapper_enters_host_mount_namespace(self):
        log = self.test_root / "nsenter-args"
        self.run_script(
            "dcgmi-wrapper.sh",
            env={"NSENTER_ARGS_FILE": str(log)},
            args=("health", "-t"),
        )
        self.assertEqual("--target 1 --mount -- dcgmi health -t\n", log.read_text())

    def test_xid_remains_unhealthy_after_deduplication(self):
        log = self.test_root / "gpu-xid.log"
        event = "kernel: NVRM: Xid (PCI:0000:01:00): 79, pid=1234"
        env = {"GPU_XID_LOGFILE": str(log), "JOURNALCTL_OUTPUT": event}
        first = self.run_script("check_gpu_xid.sh", expected=1, env=env)
        self.assertIn("GPU Xid errors detected", first)
        second = self.run_script("check_gpu_xid.sh", expected=1, env=env)
        self.assertIn("XID 79 already logged", second)


if __name__ == "__main__":
    unittest.main()
