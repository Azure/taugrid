# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase

ALLOWED = '("96.00.BC.00.02" "96.00.BC.00.01")'


class GpuVbiosTests(ScriptTestCase):
    def run_allow_list(self, versions, expected=0, allow_list=ALLOWED):
        return self.run_script(
            "check_gpu_vbios.sh",
            expected=expected,
            env={
                "NVIDIA_SMI_VBIOS_VERSIONS": versions,
                "VBIOS_VERSIONS": allow_list,
            },
        )

    def run_consistency(self, versions, expected=0, extra_env=None):
        return self.run_script(
            "check_gpu_vbios_consistency.sh",
            expected=expected,
            env={"NVIDIA_SMI_VBIOS_VERSIONS": versions, **(extra_env or {})},
        )

    def test_uniform_allowed_vbios_passes_both_checks(self):
        versions = "96.00.BC.00.02 96.00.BC.00.02"
        self.assertIn("matches one of the expected versions", self.run_allow_list(versions))
        self.assertIn(
            "All GPUs report the same VBIOS version",
            self.run_consistency(versions),
        )

    def test_uniform_unexpected_vbios_is_drift_only(self):
        versions = "96.00.BC.00.99 96.00.BC.00.99"
        drift = self.run_allow_list(versions, expected=1)
        self.assertIn("does not match one of the expected versions", drift)
        self.assertIn("96.00.BC.00.99", drift)
        self.assertIn("FaultCode: NHC2001", drift)
        self.assertNotIn("More than 1 VBIOS version", drift)
        self.assertIn(
            "All GPUs report the same VBIOS version",
            self.run_consistency(versions),
        )

    def test_mixed_allowed_vbios_is_consistency_fault_only(self):
        versions = "96.00.BC.00.02 96.00.BC.00.01"
        fault = self.run_consistency(versions, expected=1)
        self.assertIn("More than 1 VBIOS version", fault)
        self.assertIn("96.00.BC.00.01", fault)
        self.assertIn("96.00.BC.00.02", fault)
        self.assertIn("FaultCode: NHC2001", fault)
        self.assertNotIn("does not match one of the expected versions", fault)
        self.assertIn("matches one of the expected versions", self.run_allow_list(versions))

    def test_mixed_unexpected_vbios_fails_allow_list_only(self):
        output = self.run_allow_list(
            "96.00.BC.00.02 96.00.BC.00.99", expected=1
        )
        self.assertIn(
            "GPU VBIOS version (96.00.BC.00.99) does not match", output
        )
        self.assertNotIn("More than 1 VBIOS version", output)

    def test_empty_allow_list_does_not_disable_consistency(self):
        skipped = self.run_allow_list(
            "96.00.BC.00.99 96.00.BC.00.99", allow_list='("")'
        )
        self.assertIn("No expected VBIOS versions configured, skipping check", skipped)
        inconsistent = self.run_consistency(
            "96.00.BC.00.02 96.00.BC.00.01",
            expected=1,
            extra_env={"VBIOS_VERSIONS": '("")'},
        )
        self.assertIn("More than 1 VBIOS version", inconsistent)

    def test_query_failures_are_unknown_for_both_checks(self):
        for script, env in (
            ("check_gpu_vbios.sh", {"VBIOS_VERSIONS": ALLOWED}),
            ("check_gpu_vbios_consistency.sh", {}),
        ):
            with self.subTest(script=script):
                output = self.run_script(
                    script,
                    expected=2,
                    env={**env, "NVIDIA_SMI_QUERY_FAIL": "1"},
                )
                self.assertIn("failed to run nvidia-smi", output)

    def test_missing_vbios_is_unknown_for_both_checks(self):
        for script, env in (
            ("check_gpu_vbios.sh", {"VBIOS_VERSIONS": ALLOWED}),
            ("check_gpu_vbios_consistency.sh", {}),
        ):
            with self.subTest(script=script):
                output = self.run_script(
                    script,
                    expected=2,
                    env={**env, "NVIDIA_SMI_NO_VBIOS": "1"},
                )
                self.assertIn("No VBIOS version found", output)


if __name__ == "__main__":
    unittest.main()
