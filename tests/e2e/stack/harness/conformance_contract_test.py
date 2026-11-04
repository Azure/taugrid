# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import conformance_contract as contract


def node(gpus=8, ready=True, unschedulable=False, instance_type="gpu-type", series="gpu-series"):
    return {
        "metadata": {"name": "worker", "labels": {
            "node.kubernetes.io/instance-type": instance_type,
            "kueue.azure.com/gpu-series": series,
        }},
        "spec": {"unschedulable": unschedulable},
        "status": {"allocatable": {"nvidia.com/gpu": str(gpus)},
                   "conditions": [{"type": "Ready", "status": "True" if ready else "False"}]},
    }


class ContractTests(unittest.TestCase):
    def test_duration_and_budget(self):
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(contract.execution_budget(False), 250)
            self.assertEqual(contract.execution_budget(True), 250)
        for value in ("0", "0s", "-1m", "91m", "NaN", "1m junk", "1e2s"):
            with self.subTest(value=value), patch.dict(os.environ, {"DURATION": value}, clear=True):
                with self.assertRaises(ValueError):
                    contract.duration_minutes("DURATION", "30m", 90)
        for value, expected in (("1h30m", 90), ("+30m", 30), ("100ms", 1 / 600)):
            with patch.dict(os.environ, {"DURATION": value}, clear=True):
                self.assertAlmostEqual(contract.duration_minutes("DURATION", "30m", 90), expected)
        env = {"CONFORMANCE_ADMISSION_TIMEOUT": "90m", "FINEWEB_RAYJOB_TIMEOUT_MINUTES": "180",
               "FINEWEB_CHECKPOINT_CAPTURE_TIMEOUT": "30m"}
        with patch.dict(os.environ, env, clear=True):
            with self.assertRaises(ValueError):
                contract.execution_budget(True)
        env["CONFORMANCE_GO_TEST_TIMEOUT"] = "365m"
        with patch.dict(os.environ, env, clear=True), self.assertRaises(ValueError):
            contract.execution_budget(True)
        env["CONFORMANCE_ADMISSION_TIMEOUT"] = "75m"
        with patch.dict(os.environ, env, clear=True):
            self.assertEqual(contract.execution_budget(True), 370)
        for key, value in [("CONFORMANCE_GO_TEST_TIMEOUT", "366m"),
                           ("FINEWEB_CHECKPOINT_CAPTURE_TIMEOUT", "31m"),
                           ("FINEWEB_RAYJOB_TIMEOUT_MINUTES", "181")]:
            with patch.dict(os.environ, {key: value}, clear=True), self.assertRaises(ValueError):
                contract.execution_budget(True)

    def test_capacity_ignores_temporary_reservations(self):
        nodes = {"items": [node(), node()], "pods": [{"requests": {"nvidia.com/gpu": 16}}]}
        with contextlib.redirect_stdout(io.StringIO()):
            contract.capacity(nodes, "fineweb", 16, 16)
            contract.capacity({"items": [node(2)]}, "large", 2, 2)
        for items in ([node(16)], [node(), node(ready=False)], [node(), node(unschedulable=True)]):
            with self.subTest(items=items), self.assertRaises(ValueError):
                contract.capacity({"items": items}, "fineweb", 16, 16)
        with self.assertRaises(ValueError):
            contract.capacity(nodes, "large", 16, 8)

    def test_flavor_labels_fail_both_mismatch_directions(self):
        with contextlib.redirect_stdout(io.StringIO()):
            contract.gpu_labels({"items": [node()]}, "gpu-type", "gpu-series")
        for items in ([], [node(series="other")], [node(instance_type="other")],
                      [node(), node(instance_type="other")]):
            with self.subTest(items=items), self.assertRaises(ValueError):
                contract.gpu_labels({"items": items}, "gpu-type", "gpu-series")

    def test_image_version_contract(self):
        self.assertEqual(contract.ray_image("registry.example/ray:py3.12-ray2.53.0-cuda12.8", ""), "2.53.0")
        self.assertEqual(contract.ray_image("registry.example/ray@sha256:" + "0" * 64, "2.53.0"), "2.53.0")
        for image, version in [("ray:latest", ""), ("ray:ray2.53.0", "2.54.0"),
                               ("ray:latest\nforged=true", "2.53.0"), ("ray:latest", "latest")]:
            with self.subTest(image=image), self.assertRaises(ValueError):
                contract.ray_image(image, version)

    def test_selected_test_must_run_pass_and_not_skip(self):
        selected = "TestSelected"
        valid = [{"Action": "run", "Test": selected}, {"Action": "pass", "Test": selected},
                 {"Action": "pass"}]
        def verify(events):
            contract.results((json.dumps(event) for event in events), selected)
        verify(valid)
        for events in [[], valid[1:], valid[:-1], valid + [valid[1]],
                       [valid[0], {"Action": "skip", "Test": selected}, valid[2]],
                       valid + [{"Action": "fail", "Test": selected + "/child"}]]:
            with self.subTest(events=events), self.assertRaises(ValueError):
                verify(events)
        with self.assertRaises(ValueError):
            contract.results(["not JSON"], selected)

    def test_shell_guard_is_fail_closed_and_preserves_manager_mode(self):
        root = Path(__file__).resolve().parent
        for script, selected, access in [
            ("large_gpu_conformance.sh", "TestNanoGPTRayTrainLargeGPU", "LARGE_GPU_WORKLOAD_ACCESS_MODE"),
            ("fineweb_ib_conformance.sh", "TestFineWebRayTrain16xH200IB", "FINEWEB_WORKLOAD_ACCESS_MODE"),
        ]:
            for action, expected in [("pass", 0), ("skip", 1), ("missing", 1), ("go-failure", 1)]:
                body = r'''
source "$1"
assert_workload_access_contract() { :; }
assert_workload_execution_contract() { :; }
resolved_workload_namespace() { printf '%s\n' test-workloads; }
workload_kubeconfig() { printf '%s\n' unused; }
workload_context() { printf '%s\n' unused; }
delete_nanogpt_rayjob() { printf '%s\n' CLEANUP; }
delete_fineweb_rayjob() { printf '%s\n' CLEANUP; }
go() {
  case "$*" in *"-json -timeout 245m"*) ;; *) return 2 ;; esac
  if [ "$ACTION" = go-failure ]; then return 7; fi
  if [ "$ACTION" != missing ]; then
    printf '{"Action":"run","Test":"%s"}\n' "$SELECTED"
    printf '{"Action":"%s","Test":"%s"}\n' "$ACTION" "$SELECTED"
  fi
  printf '{"Action":"pass"}\n'
}
env() { while [ "$1" = -u ]; do shift 2; done; while [[ "$1" == *=* ]]; do export "$1"; shift; done; "$@"; }
run_conformance
'''
                env = {k: v for k, v in os.environ.items() if not k.startswith(("CONFORMANCE_", "FINEWEB_", "LARGE_GPU_"))}
                env.update(ACTION=action, SELECTED=selected, **{access: "manager"})
                result = subprocess.run(["bash", "-c", body, "test", str(root / script)],
                                        env=env, capture_output=True, text=True)
                with self.subTest(script=script, action=action):
                    self.assertEqual(result.returncode == 0, expected == 0, result.stderr)
                    self.assertIn("CLEANUP", result.stdout)

    def test_shell_label_checks_are_read_only(self):
        root = Path(__file__).resolve().parent
        body = r'''
source "$1"
require_platform_prep_mode() { printf '%s\n' assert-only; }
platform_kubectl() {
  if [ "$*" != "--request-timeout=30s get nodes -o json" ]; then
    printf '%s\n' "unexpected mutation: $*" >&2
    return 3
  fi
  printf '%s\n' "$NODES_JSON"
}
export GPU_INSTANCE_TYPE=gpu-type
export GPU_SERIES=gpu-series
ensure_managed_flavor_labels
'''
        for script in ("large_gpu_conformance.sh", "fineweb_ib_conformance.sh"):
            result = subprocess.run(
                ["bash", "-c", body, "test", str(root / script)],
                env={**os.environ, "NODES_JSON": json.dumps({"items": [node()]})},
                capture_output=True, text=True,
            )
            with self.subTest(script=script):
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("node/worker", result.stdout)

    def test_fineweb_caller_cleanup_owner(self):
        root = Path(__file__).resolve().parent
        body = r'''
source "$1"
assert_workload_access_contract() { :; }
resolved_workload_namespace() { printf '%s\n' test-workloads; }
workload_kubeconfig() { printf '%s\n' unused; }
workload_context() { printf '%s\n' unused; }
delete_fineweb_rayjob() { printf '%s\n' UNEXPECTED_CLEANUP; }
go() {
  printf '%s\n' '{"Action":"run","Test":"TestFineWebRayTrain16xH200IB"}' \
    '{"Action":"pass","Test":"TestFineWebRayTrain16xH200IB"}' '{"Action":"pass"}'
}
run_conformance
'''
        env = {k: v for k, v in os.environ.items() if not k.startswith(("CONFORMANCE_", "FINEWEB_"))}
        env["FINEWEB_CLEANUP_OWNER"] = "caller"
        result = subprocess.run(
            ["bash", "-c", body, "test", str(root / "fineweb_ib_conformance.sh")],
            env=env, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("UNEXPECTED_CLEANUP", result.stdout)

    def test_fineweb_capacity_check_is_read_only(self):
        root = Path(__file__).resolve().parent
        body = r'''
source "$1"
assert_platform_access_contract() { :; }
delete_fineweb_rayjob() { printf '%s\n' UNEXPECTED_DELETE; return 3; }
platform_kubectl() {
  if [ "$*" != "--request-timeout=30s get nodes -l gpu=selected -o json" ]; then
    printf '%s\n' "unexpected mutation: $*" >&2
    return 3
  fi
  printf '%s\n' "$NODES_JSON"
}
export GPU_SELECTOR_EXPR=gpu=selected
assert_capacity
'''
        env = {k: v for k, v in os.environ.items() if not k.startswith("FINEWEB_")}
        env["NODES_JSON"] = json.dumps({"items": [node(), node()]})
        result = subprocess.run(
            ["bash", "-c", body, "test", str(root / "fineweb_ib_conformance.sh")],
            env=env, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("UNEXPECTED_DELETE", result.stdout)


if __name__ == "__main__":
    unittest.main()
