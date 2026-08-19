# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import unittest

from helpers import ScriptTestCase


class GpuRuntimeTests(ScriptTestCase):
    def test_gpu_simulation_env_cannot_bypass_real_check(self):
        output = self.run_script(
            "check-nvidia-smi.sh",
            expected=1,
            env={
                "NPD_GPU_REQUIRED": "1",
                "NPD_GPU_SIMULATION": "healthy",
                "NVIDIA_SMI_LIST_FAIL": "1",
            },
        )
        self.assertIn("Failed to initialize NVML", output)
        self.assertNotIn("GPU-FAKE", output)

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

    def enable_dcgm_watches(self):
        self.dcgm_state_file.write_text("a\n")

    def test_dcgm_remote_profile_skips_host_commands(self):
        skipped = self.run_script(
            "check-dcgm-health.sh", expected=2, env={"NPD_DCGM_REQUIRED": "0"}
        )
        self.assertIn("host diagnostic not applicable", skipped)
        self.assertIn("exporter availability is reported separately", skipped)
        self.assertFalse(self.dcgm_command_log.exists())

        npd_args = self.test_root / "npd-args"
        self.run_script(
            "start-node-problem-detector.sh",
            env={
                "NPD_DCGM_REQUIRED": "0",
                "NPD_BINARY": str(self.mock_bin / "node-problem-detector"),
                "NPD_ARGS_FILE": str(npd_args),
            },
            args=("--logtostderr",),
        )
        self.assertFalse(self.dcgm_command_log.exists())
        self.assertEqual("--logtostderr\n", npd_args.read_text())

    def test_dcgm_init_sets_watches_and_waits_before_first_check(self):
        sleep_log = self.test_root / "sleep-args"
        output = self.run_script(
            "init-dcgm-health.sh",
            env={
                "NPD_DCGM_REQUIRED": "1",
                "SLEEP_ARGS_FILE": str(sleep_log),
            },
        )
        self.assertIn("dcgm health watches initialized", output)
        self.assertEqual("60\n", sleep_log.read_text())
        self.assertEqual(
            ["health -s a", "health -f", "health -c", "health -f"],
            self.dcgm_command_log.read_text().splitlines(),
        )

    def test_dcgm_init_set_failure_is_retriable_and_never_checks(self):
        output = self.run_script(
            "init-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_SET_FAIL": "1",
                "SLEEP_ARGS_FILE": str(self.test_root / "sleep-args"),
            },
        )
        self.assertIn("failed to enable dcgm health watches", output)
        self.assertEqual(
            ["health -s a"], self.dcgm_command_log.read_text().splitlines()
        )
        self.assertFalse(self.dcgm_state_file.exists())

    def test_dcgm_init_fails_when_watches_are_lost_during_warmup(self):
        output = self.run_script(
            "init-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_DROP_WATCHES_DURING_SLEEP": "1",
            },
        )
        self.assertIn("Health watches not enabled", output)
        self.assertIn("unavailable after warmup", output)
        self.assertEqual(
            ["health -s a", "health -f", "health -c"],
            self.dcgm_command_log.read_text().splitlines(),
        )

    def test_dcgm_init_revalidates_full_mask_after_warmup_check(self):
        output = self.run_script(
            "init-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "NPD_DCGM_WARMUP_SECONDS": "0",
                "DCGMI_PARTIAL_WATCHES_AFTER_CHECK": "1",
            },
        )
        self.assertIn("dcgm health watches are not fully enabled", output)
        self.assertNotIn("dcgm health watches initialized", output)
        self.assertEqual(
            ["health -s a", "health -f", "health -c", "health -f"],
            self.dcgm_command_log.read_text().splitlines(),
        )

    def test_dcgm_watch_fetch_distinguishes_off_partial_and_full_masks(self):
        required = {"NPD_DCGM_REQUIRED": "1"}
        off = self.run_script(
            "check-dcgm-watches.sh", expected=1, env=required
        )
        self.assertIn("PCIe", off)
        self.assertIn("Off", off)
        self.assertIn("not fully enabled", off)

        self.enable_dcgm_watches()
        partial = self.run_script(
            "check-dcgm-watches.sh",
            expected=1,
            env={**required, "DCGMI_PARTIAL_WATCHES": "1"},
        )
        self.assertIn("Driver", partial)
        self.assertIn("not fully enabled", partial)

        full = self.run_script("check-dcgm-watches.sh", env=required)
        self.assertIn("dcgm health watches enabled", full)

        fetch_failure = self.run_script(
            "check-dcgm-watches.sh",
            expected=1,
            env={**required, "DCGMI_FETCH_FAIL": "1"},
        )
        self.assertIn("failed to fetch dcgm health watches", fetch_failure)

    def test_dcgm_healthy_warning_failure_and_unknown_results(self):
        self.enable_dcgm_watches()
        healthy = self.run_script(
            "check-dcgm-health.sh",
            env={"NPD_DCGM_REQUIRED": "1"},
        )
        self.assertIn("| Overall Health | Healthy |", healthy)

        for status in ("Warning", "Failure"):
            with self.subTest(status=status):
                output = self.run_script(
                    "check-dcgm-health.sh",
                    expected=1,
                    env={
                        "NPD_DCGM_REQUIRED": "1",
                        "DCGMI_HEALTH_OUTPUT": (
                            "Health Monitor Report\n"
                            f"| Overall Health | {status} |\n"
                            "Incident: GPU 0 memory fault"
                        ),
                    },
                )
                self.assertNotIn(f"| Overall Health | {status} |", output)
                self.assertIn(f"{status}=1", output)

        unknown = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_HEALTH_OUTPUT": "Health Monitor Report",
            },
        )
        self.assertIn("rows=0", unknown)

        empty = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_HEALTH_OUTPUT": "",
            },
        )
        self.assertIn("returned no result", empty)

    def test_dcgm_health_accepts_only_one_recognized_healthy_row(self):
        self.enable_dcgm_watches()
        valid_outputs = (
            (
                "Health monitor systems report\n"
                "+----------------+---------+\n"
                "| Overall Health | Healthy |\n"
                "+----------------+---------+"
            ),
            "Health monitor report\n|   OVERALL HEALTH   |   hEaLtHy   |",
        )
        for output in valid_outputs:
            with self.subTest(valid=output):
                result = self.run_script(
                    "check-dcgm-health.sh",
                    env={
                        "NPD_DCGM_REQUIRED": "1",
                        "DCGMI_HEALTH_OUTPUT": output,
                    },
                )
                self.assertIn("Healthy", result.lower().title())

        invalid_outputs = (
            (
                "duplicate healthy",
                "| Overall Health | Healthy |\n| Overall Health | Healthy |",
            ),
            (
                "healthy warning conflict",
                "| Overall Health | Healthy |\n| Overall Health | Warning |",
            ),
            (
                "healthy failure conflict",
                "| Overall Health | Healthy |\n| Overall Health | Failure |",
            ),
            (
                "same-row status conflict",
                "| Overall Health | Healthy Warning |",
            ),
            (
                "prefixed label",
                "| Not Overall Health | Healthy |",
            ),
            (
                "suffixed label",
                "| Overall Health Status | Healthy |",
            ),
            (
                "extra column",
                "| Overall Health | Healthy | unexpected |",
            ),
            (
                "multiple statuses on one line",
                "| Overall Health | Failure | Overall Health | Healthy |",
            ),
            (
                "numeric status suffix",
                "| Overall Health | Healthy123 |",
            ),
            (
                "separated numeric suffix",
                "| Overall Health | Healthy 123 |",
            ),
            (
                "colon delimiter",
                "Overall Health: Healthy",
            ),
            (
                "missing trailing delimiter",
                "| Overall Health | Healthy",
            ),
        )
        for label, output in invalid_outputs:
            with self.subTest(invalid=label):
                result = self.run_script(
                    "check-dcgm-health.sh",
                    expected=1,
                    env={
                        "NPD_DCGM_REQUIRED": "1",
                        "DCGMI_HEALTH_OUTPUT": output,
                    },
                )
                self.assertIn("rejected Overall Health result set", result)
                self.assertNotIn(output, result)

    def test_dcgm_health_rejects_partial_mask_after_healthy_check(self):
        self.enable_dcgm_watches()
        output = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_PARTIAL_WATCHES_AFTER_CHECK": "1",
            },
        )
        self.assertIn("dcgm health watches are not fully enabled", output)
        self.assertNotIn("| Overall Health | Healthy |", output)
        self.assertEqual(
            ["health -c", "health -f"],
            self.dcgm_command_log.read_text().splitlines(),
        )

    def test_dcgm_nonzero_and_uninitialized_results_fail_closed(self):
        no_watches = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={"NPD_DCGM_REQUIRED": "1"},
        )
        self.assertIn("Health watches not enabled", no_watches)
        self.assertIn("return code 253", no_watches)

        simulated_healthy = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "NPD_DCGM_SIMULATION": "healthy",
            },
        )
        self.assertIn("Health watches not enabled", simulated_healthy)

        self.enable_dcgm_watches()
        command_failure = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={
                "NPD_DCGM_REQUIRED": "1",
                "DCGMI_HEALTH_OUTPUT": "DCGM transport failure",
                "DCGMI_HEALTH_EXIT_CODE": "3",
            },
        )
        self.assertIn("DCGM transport failure", command_failure)
        self.assertIn("return code 3", command_failure)

    def test_dcgm_host_engine_restart_requires_reinitialization(self):
        init_env = {
            "NPD_DCGM_REQUIRED": "1",
            "NPD_DCGM_WARMUP_SECONDS": "0",
        }
        self.run_script("init-dcgm-health.sh", env=init_env)
        self.dcgm_command_log.unlink()
        npd_args = self.test_root / "npd-args"
        start_env = {
            **init_env,
            "NPD_BINARY": str(self.mock_bin / "node-problem-detector"),
            "NPD_ARGS_FILE": str(npd_args),
        }
        self.run_script(
            "start-node-problem-detector.sh",
            env=start_env,
            args=("--logtostderr", "--port=20260"),
        )
        self.assertEqual(
            ["health -f"], self.dcgm_command_log.read_text().splitlines()
        )
        self.assertEqual("--logtostderr --port=20260\n", npd_args.read_text())

        self.dcgm_state_file.unlink()

        lost_watches = self.run_script(
            "check-dcgm-watches.sh",
            expected=1,
            env={"NPD_DCGM_REQUIRED": "1"},
        )
        self.assertIn("not fully enabled", lost_watches)
        lost_health = self.run_script(
            "check-dcgm-health.sh",
            expected=1,
            env={"NPD_DCGM_REQUIRED": "1"},
        )
        self.assertIn("Health watches not enabled", lost_health)

        self.run_script("start-node-problem-detector.sh", env=start_env)
        self.run_script(
            "check-dcgm-watches.sh",
            env={"NPD_DCGM_REQUIRED": "1"},
        )
        self.run_script(
            "check-dcgm-health.sh",
            env={"NPD_DCGM_REQUIRED": "1"},
        )
        commands = self.dcgm_command_log.read_text().splitlines()
        self.assertEqual(1, commands.count("health -s a"))
        self.assertNotIn("health -t", commands)

    def test_dcgm_repeated_checks_never_mutate_watch_configuration(self):
        self.enable_dcgm_watches()
        env = {"NPD_DCGM_REQUIRED": "1"}
        self.run_script("check-dcgm-health.sh", env=env)
        self.run_script("check-dcgm-health.sh", env=env)
        self.assertEqual(
            ["health -c", "health -f", "health -c", "health -f"],
            self.dcgm_command_log.read_text().splitlines(),
        )

    def test_dcgm_wrapper_enters_host_mount_namespace(self):
        log = self.test_root / "nsenter-args"
        self.run_script(
            "dcgmi-wrapper.sh",
            env={"NSENTER_ARGS_FILE": str(log)},
            args=("health", "-c"),
        )
        self.assertEqual("--target 1 --mount -- dcgmi health -c\n", log.read_text())

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
