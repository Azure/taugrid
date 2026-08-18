# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase


class GpuDriverTests(ScriptTestCase):
    def driver_env(self, expected=None):
        driver_root = self.test_root / "proc" / "driver" / "nvidia"
        driver_root.mkdir(parents=True, exist_ok=True)
        env = {"NVIDIA_DRIVER_ROOT": str(driver_root)}
        if expected is not None:
            env["CREATE_IMEX_CHANNEL_EXPECTED"] = expected
        return env

    def write_params(self, value):
        params = self.test_root / "proc" / "driver" / "nvidia" / "params"
        params.write_text(f"CreateImexChannel0: {value}\n")

    def test_driver_missing_fails(self):
        output = self.run_script(
            "check_gpu_driver.sh",
            expected=1,
            env={
                "NVIDIA_DRIVER_ROOT": str(self.test_root / "missing-driver"),
            },
        )
        self.assertIn("driver not loaded", output)

    def test_configured_parameter_missing_fails(self):
        output = self.run_script(
            "check_gpu_driver.sh", expected=1, env=self.driver_env("0")
        )
        self.assertIn("CreateImexChannel0 was not found", output)

    def test_a100_expected_zero_passes(self):
        env = self.driver_env("0")
        self.write_params("0")
        output = self.run_script("check_gpu_driver.sh", env=env)
        self.assertIn("CreateImexChannel0 is 0 as expected", output)

    def test_expected_one_passes(self):
        env = self.driver_env("1")
        self.write_params("1")
        output = self.run_script("check_gpu_driver.sh", env=env)
        self.assertIn("CreateImexChannel0 is 1 as expected", output)

    def test_explicit_params_file_is_injectable(self):
        env = self.driver_env("0")
        params = self.test_root / "injected-params"
        params.write_text("CreateImexChannel0: 0\n")
        env["NVIDIA_DRIVER_PARAMS_FILE"] = str(params)
        self.run_script("check_gpu_driver.sh", env=env)

    def test_explicit_mismatches_fail(self):
        for expected, actual in (("0", "1"), ("1", "0")):
            with self.subTest(expected=expected, actual=actual):
                env = self.driver_env(expected)
                self.write_params(actual)
                output = self.run_script(
                    "check_gpu_driver.sh", expected=1, env=env
                )
                self.assertIn(
                    f"CreateImexChannel0' is {actual} (expected {expected}", output
                )

    def test_invalid_expectation_fails_closed(self):
        output = self.run_script(
            "check_gpu_driver.sh",
            expected=1,
            env={"CREATE_IMEX_CHANNEL_EXPECTED": "required"},
        )
        self.assertIn("must be empty, 0, or 1", output)

    def test_legacy_present_one_passes(self):
        env = self.driver_env()
        self.write_params("1")
        output = self.run_script("check_gpu_driver.sh", env=env)
        self.assertIn("legacy contract", output)

    def test_legacy_present_zero_fails(self):
        env = self.driver_env()
        self.write_params("0")
        output = self.run_script("check_gpu_driver.sh", expected=1, env=env)
        self.assertIn("expected 1", output)

    def test_legacy_missing_parameter_passes(self):
        output = self.run_script("check_gpu_driver.sh", env=self.driver_env())
        self.assertIn("not exposed by this driver", output)


if __name__ == "__main__":
    unittest.main()
