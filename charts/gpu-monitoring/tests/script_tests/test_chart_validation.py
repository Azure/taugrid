# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import re
import subprocess
import tempfile
import unittest
from pathlib import Path

from helpers import CHART_DIR


RESERVED_ENV_NAMES = [
    "NODE_NAME",
    "NPD_GPU_REQUIRED",
    "NPD_DCGM_REQUIRED",
    "EXPECTED_NUM_GPU",
    "VBIOS_VERSIONS",
    "IB_DEVICES",
    "EXPECTED_IB_PKEY",
    "EXPECTED_IB_GBPS",
    "IB_FLAP_THRESHOLD_SHORT",
    "IB_FLAP_CHECK_WINDOW",
    "NVME_TOTAL",
    "NVME_SIZE_COUNT",
    "NVME_SIZE",
    "IMEX_PROCNAME",
    "CREATE_IMEX_CHANNEL_EXPECTED",
    "ROCE_DEVICES",
    "GPU_TYPE",
    "GPU_DRIVER_VERSIONS",
    "NPD_BINARY",
    "NPD_DCGM_WARMUP_SECONDS",
    "NPD_DCGM_SIMULATION",
    "NPD_GPU_SIMULATION",
]


def render(values, *, daemonset_only=True):
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".yaml", encoding="utf-8"
    ) as values_file:
        values_file.write(values)
        values_file.flush()
        command = [
            "helm",
            "template",
            "chart-validation",
            str(CHART_DIR),
            "--values",
            values_file.name,
        ]
        if daemonset_only:
            command.extend(["--show-only", "templates/daemonset.yaml"])
        return subprocess.run(command, capture_output=True, text=True)


class ChartValidationTests(unittest.TestCase):
    def test_reserved_environment_names_are_centralized_and_rejected(self):
        helpers = (CHART_DIR / "templates" / "_helpers.tpl").read_text()
        block = re.search(
            r'define "gpu-monitoring\.reservedEnvNames" -}}\n(.*?)\n{{- end }}',
            helpers,
            re.S,
        )
        self.assertIsNotNone(block)
        self.assertEqual(
            RESERVED_ENV_NAMES,
            re.findall(r"^- ([A-Z][A-Z0-9_]*)$", block.group(1), re.M),
        )

        for name in RESERVED_ENV_NAMES:
            with self.subTest(name=name):
                result = render(
                    f"""
enabledGpuSkus:
  - h200
env:
  - name: {name}
    value: custom
"""
                )
                self.assertNotEqual(0, result.returncode)
                self.assertIn(
                    f"env must not override reserved variable {name}", result.stderr
                )

    def test_duplicate_custom_environment_name_is_rejected(self):
        result = render(
            """
enabledGpuSkus:
  - h200
env:
  - name: CUSTOM_ENV
    value: first
  - name: CUSTOM_ENV
    value: second
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("env contains duplicate variable CUSTOM_ENV", result.stderr)

    def test_unrelated_custom_environment_name_renders(self):
        result = render(
            """
enabledGpuSkus:
  - h200
env:
  - name: CUSTOM_ENV
    value: allowed
"""
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("name: CUSTOM_ENV", result.stdout)

    def test_profile_environment_values_cannot_inject_reserved_entries(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    ib_devices: |-
      mlx5_0:1"
      - name: NPD_DCGM_REQUIRED
        value: "0
"""
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(
            2,
            len(
                re.findall(
                    r"^\s+- name: NPD_DCGM_REQUIRED$", result.stdout, re.MULTILINE
                )
            ),
        )

    def test_collector_disabled_host_local_profile_skips_collector_validation(self):
        result = render(
            """
enabledGpuSkus:
  - h200
metricsCollector:
  enabled: false
  dcgmAvailability:
    enabled: true
    condition: invalid-condition
"""
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertNotIn("name: metrics-collector", result.stdout)

    def test_explicit_availability_condition_is_yaml_safe(self):
        result = render(
            """
enabledGpuSkus:
  - h200
metricsCollector:
  dcgmAvailability:
    enabled: false
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://localhost:19400/metrics
        required: true
        availabilityCondition: CustomExporterUnavailable
""",
            daemonset_only=False,
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn('availabilityCondition: "CustomExporterUnavailable"', result.stdout)

    def test_invalid_exporter_url_error_redacts_credentials(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: https://user:secret@host/path?token=abc#frag
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("must set an absolute lowercase HTTP(S) url", result.stderr)
        for secret in ("user", "secret", "token", "abc", "frag", "?"):
            with self.subTest(secret=secret):
                self.assertNotIn(secret, result.stderr)

    def test_host_local_url_classification_rejects_trailing_yaml_payloads(self):
        for url in (
            "http://localhost:19400/metrics: [marker]",
            "http://localhost:19400/metrics # marker",
            "http://127.0.0.1:19400/metrics: [marker]",
        ):
            with self.subTest(url=url):
                result = render(
                    f"""
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: {url!r}
"""
                )
                self.assertNotEqual(0, result.returncode)
                self.assertIn(
                    "must set an absolute lowercase HTTP(S) url", result.stderr
                )
                self.assertNotIn("marker", result.stderr)

        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: |-
          http://localhost:19400/metrics
          # marker
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn("must set an absolute lowercase HTTP(S) url", result.stderr)
        self.assertNotIn("marker", result.stderr)

    def test_host_local_url_query_and_fragment_remain_supported(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: "http://localhost:19400/metrics?selector=[gpu:type]#status"
""",
            daemonset_only=False,
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn(
            'url: "http://localhost:19400/metrics?selector=[gpu:type]#status"',
            result.stdout,
        )

    def test_exporter_urls_rejected_by_collector_are_rejected_by_helm(self):
        for url in (
            "HTTP://localhost:19400/metrics",
            "http://dcgm.svc/metrics%zz",
            "http://dcgm.svc:notaport/metrics",
            "http://[::1",
            "http://[::::]:9400/metrics",
            "http://[1:2:3]:9400/metrics",
            "http://[[::1]:9400/metrics",
            "http://dcgm%2Esvc/metrics",
            r"http://localhost:19400/metrics\secret",
            r"http://dcgm.svc/metrics\secret",
        ):
            with self.subTest(url=url):
                result = render(
                    f"""
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: {url}
"""
                )
                self.assertNotEqual(0, result.returncode)
                self.assertIn(
                    "must set an absolute lowercase HTTP(S) url", result.stderr
                )

    def test_legacy_collector_compatible_shapes_render(self):
        compatible_values = {
            "duplicate optional target names": """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
      - name: node-exporter
        url: http://localhost:9100/metrics
      - name: node-exporter
        url: http://localhost:9101/metrics
""",
            "duplicate rule conditions": """
enabledGpuSkus:
  - h200
metricsCollector:
  rules:
    - name: first
      metricName: first_metric
      conditionType: DuplicateCondition
      mode: instant
      threshold: 0
    - name: second
      metricName: second_metric
      conditionType: DuplicateCondition
      mode: instant
      threshold: 0
""",
            "empty optional scrape target": """
enabledGpuSkus:
  - h200
metricsCollector:
  dcgmAvailability:
    enabled: false
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://localhost:19400/metrics
      - {}
""",
            "explicit remote availability owner": """
enabledGpuSkus:
  - h200
metricsCollector:
  dcgmAvailability:
    enabled: false
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: https://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
        required: true
        availabilityCondition: DcgmExporterUnavailable
""",
            "absolute IPv6 remote service URL": """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://[fd00::1]:9400/metrics
""",
        }
        for shape, values in compatible_values.items():
            with self.subTest(shape=shape):
                result = render(values, daemonset_only=False)
                self.assertEqual(0, result.returncode, result.stderr)


if __name__ == "__main__":
    unittest.main()
