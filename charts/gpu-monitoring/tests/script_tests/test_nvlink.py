# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase


class NvlinkTests(ScriptTestCase):
    def test_blackwell_checks_pass(self):
        self.run_script(
            "check_gpu_nvlink_b200.sh",
            env={"GPU_TYPE": "GB300", "EXPECTED_NUM_GPU": "1"},
        )
        self.run_script("check_temp_imex.sh", env={"GPU_TYPE": "GB300"})

    def test_empty_status_fails_both_checks(self):
        common = {
            "EXPECTED_NUM_GPU": "1",
            "NVIDIA_SMI_EMPTY_NVLINK_STATUS": "1",
        }
        output = self.run_script("check_gpu_nvlink.sh", expected=1, env=common)
        self.assertIn("NVLINK is not enabled", output)

        output = self.run_script(
            "check_gpu_nvlink_b200.sh",
            expected=1,
            env={**common, "GPU_TYPE": "GB300"},
        )
        self.assertIn("NVLINK is not enabled", output)

    def test_blackwell_query_failures_are_reported(self):
        common = {"GPU_TYPE": "GB300", "EXPECTED_NUM_GPU": "1"}
        for failure in ("NVIDIA_SMI_NVLINK_DETAIL_FAIL", "NVIDIA_SMI_C2C_FAIL"):
            with self.subTest(failure=failure):
                output = self.run_script(
                    "check_gpu_nvlink_b200.sh",
                    expected=1,
                    env={**common, failure: "1"},
                )
                self.assertIn("error code 1", output)

    def test_non_blackwell_node_is_skipped(self):
        output = self.run_script(
            "check_gpu_nvlink_b200.sh",
            env={"GPU_TYPE": "H100", "EXPECTED_NUM_GPU": "1"},
        )
        self.assertIn("Not a Blackwell node", output)


if __name__ == "__main__":
    unittest.main()
