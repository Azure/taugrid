# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

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
            script = mutated_chart / "scripts/check_gpu_xid.sh"
            script.write_text(script.read_text() + "\n# content hash regression probe\n")
            mutated = render(
                "templates/executable-bundle-secret.yaml", mutated_chart
            )
            mutated_name = re.search(
                r"^  name: (gpu-monitoring-gpu-[a-f0-9]{10})$", mutated, re.M
            )
            self.assertIsNotNone(mutated_name)
            self.assertNotEqual(original_name.group(1), mutated_name.group(1))

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


if __name__ == "__main__":
    unittest.main()
