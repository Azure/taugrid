# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

import yaml

from helpers import CHART_DIR


def render(template, chart=CHART_DIR):
    return subprocess.run(
        ["helm", "template", "wiring", str(chart), "--show-only", template],
        check=True,
        capture_output=True,
        text=True,
    ).stdout


class BundleWiringTests(unittest.TestCase):
    def test_bundle_name_changes_with_script_content(self):
        original = render("templates/executable-bundle-secret.yaml")
        original_name = re.search(r"^  name: (gpu-monitoring-gpu-[a-f0-9]{10})$", original, re.M)
        self.assertIsNotNone(original_name)

        with tempfile.TemporaryDirectory() as temp:
            mutated_chart = Path(temp) / "gpu-monitoring"
            shutil.copytree(CHART_DIR, mutated_chart)
            script = mutated_chart / "scripts/init-dcgm-health.sh"
            script.write_text(script.read_text() + "\n# content hash regression probe\n")
            mutated = render(
                "templates/executable-bundle-secret.yaml", mutated_chart
            )
            mutated_name = re.search(
                r"^  name: (gpu-monitoring-gpu-[a-f0-9]{10})$", mutated, re.M
            )
            self.assertIsNotNone(mutated_name)
            self.assertNotEqual(original_name.group(1), mutated_name.group(1))

    def test_every_builtin_host_profile_initializes_and_monitors_watches(self):
        daemonsets = [
            document
            for document in render("templates/daemonset.yaml").split("---")
            if "kind: DaemonSet" in document
        ]
        self.assertEqual(13, len(daemonsets))
        for daemonset in daemonsets:
            name = re.search(r"^  name: (gpu-monitoring-[a-z0-9-]+)$", daemonset, re.M)
            self.assertIsNotNone(name)
            with self.subTest(daemonset=name.group(1)):
                self.assertIn("- name: init-dcgm-health", daemonset)
                self.assertIn("livenessProbe:", daemonset)
                self.assertIn("- /custom-config/check-dcgm-watches.sh", daemonset)
                self.assertIn("- /custom-config/start-node-problem-detector.sh", daemonset)

    def test_host_required_monitor_config_stays_byte_identical(self):
        rendered = subprocess.run(
            [
                "helm",
                "template",
                "host-config",
                str(CHART_DIR),
                "--set",
                "enabledGpuSkus[0]=h200",
            ],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        documents = [doc for doc in yaml.safe_load_all(rendered) if doc]
        secret = next(doc for doc in documents if doc["kind"] == "Secret")
        daemonset = next(doc for doc in documents if doc["kind"] == "DaemonSet")
        config_key = next(
            arg.rsplit("/", 1)[1]
            for arg in daemonset["spec"]["template"]["spec"]["containers"][0]["args"]
            if arg.startswith("--config.custom-plugin-monitor=")
        )
        self.assertEqual("custom-plugin-monitor-h100.json", config_key)
        self.assertEqual(
            (CHART_DIR / "configs" / config_key).read_text(),
            secret["stringData"][config_key],
        )

    def test_every_plugin_is_bundled_and_mounted(self):
        secret = render("templates/executable-bundle-secret.yaml")
        bundle_keys = set(re.findall(r"^  ([A-Za-z0-9._-]+): \|$", secret, re.M))
        daemonset = render("templates/daemonset.yaml")
        mounted_subpaths = set()
        volume = None
        for line in daemonset.splitlines():
            name = re.match(r"^\s+- name: ([A-Za-z0-9._-]+)$", line)
            if name:
                volume = name.group(1)
                continue
            subpath = re.match(r"^\s+subPath: ([A-Za-z0-9._-]+)$", line)
            if subpath and volume == "custom-config":
                mounted_subpaths.add(subpath.group(1))

        mounted_scripts = {
            subpath for subpath in mounted_subpaths if subpath.endswith(".sh")
        }
        referenced_plugins = {
            rule["path"].removeprefix("/custom-config/")
            for config in CHART_DIR.glob("configs/custom-plugin-monitor*.json")
            for rule in json.loads(config.read_text())["rules"]
            if rule["path"].startswith("/custom-config/")
        }
        referenced_scripts = {
            plugin for plugin in referenced_plugins if plugin.endswith(".sh")
        }

        self.assertGreaterEqual(len(mounted_scripts), 15)
        self.assertGreaterEqual(len(referenced_scripts), 15)
        self.assertLessEqual(mounted_subpaths, bundle_keys)
        self.assertLessEqual(referenced_plugins, mounted_subpaths)
        self.assertIn("check_gpu_vbios.sh", mounted_scripts)
        self.assertIn("check_gpu_vbios_consistency.sh", mounted_scripts)

    def test_every_profile_wires_both_vbios_conditions(self):
        configs = list(CHART_DIR.glob("configs/custom-plugin-monitor*.json"))
        self.assertEqual(5, len(configs))
        for path in configs:
            with self.subTest(config=path.name):
                config = json.loads(path.read_text())
                conditions = {condition["type"] for condition in config["conditions"]}
                permanent_rules = {
                    (rule.get("condition"), rule["path"])
                    for rule in config["rules"]
                    if rule["type"] == "permanent"
                }
                self.assertIn("GPUVbiosMismatch", conditions)
                self.assertIn("GPUVbiosInconsistent", conditions)
                self.assertIn(
                    ("GPUVbiosMismatch", "/custom-config/check_gpu_vbios.sh"),
                    permanent_rules,
                )
                self.assertIn(
                    (
                        "GPUVbiosInconsistent",
                        "/custom-config/check_gpu_vbios_consistency.sh",
                    ),
                    permanent_rules,
                )

    def test_remote_profile_keeps_permanent_dcgm_unknown_transition(self):
        values = yaml.safe_load((CHART_DIR / "values.yaml").read_text())
        self.assertEqual("v0.8.19", values["image"]["tag"])
        rendered = subprocess.run(
            [
                "helm",
                "template",
                "remote-wiring",
                str(CHART_DIR),
                "--values",
                str(CHART_DIR / "tests" / "mixed-dcgm-values.yaml"),
                "--set",
                "enabledGpuSkus[0]=h200",
            ],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        documents = [doc for doc in yaml.safe_load_all(rendered) if doc]
        secret = next(doc for doc in documents if doc["kind"] == "Secret")
        daemonset = next(doc for doc in documents if doc["kind"] == "DaemonSet")
        config_arg = next(
            arg
            for arg in daemonset["spec"]["template"]["spec"]["containers"][0]["args"]
            if arg.startswith("--config.custom-plugin-monitor=")
        )
        config_key = config_arg.rsplit("/", 1)[1]
        config = json.loads(secret["stringData"][config_key])

        condition_types = {condition["type"] for condition in config["conditions"]}
        self.assertIn("DcgmHealthProblem", condition_types)
        dcgm_rules = [
            rule
            for rule in config["rules"]
            if rule["path"] == "/custom-config/check-dcgm-health.sh"
        ]
        self.assertEqual(1, len(dcgm_rules))
        self.assertEqual("permanent", dcgm_rules[0]["type"])
        self.assertEqual("DcgmHealthProblem", dcgm_rules[0]["condition"])
        self.assertFalse(
            any(rule["type"] == "temporary" for rule in dcgm_rules)
        )
        self.assertLessEqual(
            {
                rule["condition"]
                for rule in config["rules"]
                if rule["type"] == "permanent"
            },
            condition_types,
        )

    def test_host_local_profile_with_dcgm_disabled_omits_dcgm_claims(self):
        rendered = subprocess.run(
            [
                "helm",
                "template",
                "disabled-dcgm-wiring",
                str(CHART_DIR),
                "--set",
                "enabledGpuSkus[0]=h200",
                "--set",
                "gpuSkus.h200.dcgm_health_required=false",
            ],
            check=True,
            capture_output=True,
            text=True,
        ).stdout
        documents = [doc for doc in yaml.safe_load_all(rendered) if doc]
        secret = next(doc for doc in documents if doc["kind"] == "Secret")
        daemonset = next(doc for doc in documents if doc["kind"] == "DaemonSet")
        container = daemonset["spec"]["template"]["spec"]["containers"][0]
        config_key = next(
            arg.rsplit("/", 1)[1]
            for arg in container["args"]
            if arg.startswith("--config.custom-plugin-monitor=")
        )
        config = json.loads(secret["stringData"][config_key])

        self.assertIn(
            {"name": "NPD_DCGM_REQUIRED", "value": "0"}, container["env"]
        )
        self.assertIn(
            "DcgmHealthProblem",
            {condition["type"] for condition in config["conditions"]},
        )
        dcgm_rules = [
            rule
            for rule in config["rules"]
            if rule["path"] == "/custom-config/check-dcgm-health.sh"
        ]
        self.assertEqual(1, len(dcgm_rules))
        self.assertEqual("permanent", dcgm_rules[0]["type"])


if __name__ == "__main__":
    unittest.main()
