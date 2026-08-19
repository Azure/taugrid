# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import re
import subprocess
import tempfile
import unittest

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

LEGACY_KEY_MIGRATION = (
    'migrate to dcgmHealth.source and dcgmHealth.exporterUrl (see the README '
    '"DCGM health sources" section)'
)


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

    def test_host_dcgmi_profile_requires_the_metrics_collector(self):
        result = render(
            """
enabledGpuSkus:
  - h200
metricsCollector:
  enabled: false
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            "gpuSkus.h200 uses dcgmHealth.source=host-dcgmi and requires "
            "metricsCollector.enabled=true",
            result.stderr,
        )

    def test_default_source_is_host_dcgmi(self):
        result = render(
            """
enabledGpuSkus:
  - h200
""",
            daemonset_only=False,
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn('url: "http://localhost:19400/metrics"', result.stdout)

    def test_exporter_source_with_explicit_exporter_url_renders(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      source: exporter
      exporterUrl: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
""",
            daemonset_only=False,
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn(
            'url: "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"',
            result.stdout,
        )

    def test_invalid_source_is_rejected(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      source: gpu-operator
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            'gpuSkus.h200 effective dcgmHealth.source must be exactly one of '
            '"host-dcgmi" or "exporter"',
            result.stderr,
        )

    def test_null_global_dcgm_health_fails_loud_without_a_reflect_panic(self):
        result = render(
            """
enabledGpuSkus:
  - h200
dcgmHealth: null
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertNotIn("reflect", result.stderr)
        self.assertIn(
            "gpuSkus.h200: global dcgmHealth must be a map with source "
            "and/or exporterUrl keys; got null",
            result.stderr,
        )

    def test_scalar_global_dcgm_health_fails_loud_without_a_reflect_panic(self):
        result = render(
            """
enabledGpuSkus:
  - h200
dcgmHealth: bogus
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertNotIn("reflect", result.stderr)
        self.assertIn(
            "gpuSkus.h200: global dcgmHealth must be a map with source "
            "and/or exporterUrl keys; got string",
            result.stderr,
        )

    def test_null_per_profile_dcgm_health_fails_loud_without_a_reflect_panic(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth: null
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertNotIn("reflect", result.stderr)
        self.assertIn(
            "gpuSkus.h200.dcgmHealth must be a map with source and/or "
            "exporterUrl keys; got null",
            result.stderr,
        )

    def test_scalar_per_profile_dcgm_health_fails_loud_without_a_reflect_panic(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth: bogus
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertNotIn("reflect", result.stderr)
        self.assertIn(
            "gpuSkus.h200.dcgmHealth must be a map with source and/or "
            "exporterUrl keys; got string",
            result.stderr,
        )

    def test_invalid_exporter_url_error_redacts_credentials(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      source: exporter
      exporterUrl: http://user:secret@dcgm.svc/path?token=abc#frag
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            "must be a nonempty absolute lowercase http(s) URL", result.stderr
        )
        for secret in ("user", "secret", "token", "abc", "frag", "?"):
            with self.subTest(secret=secret):
                self.assertNotIn(secret, result.stderr)

    def test_exporter_urls_rejected_by_the_collector_are_rejected_by_helm(self):
        for url in (
            "HTTP://dcgm.svc:9400/metrics",
            "http://dcgm.svc/metrics%zz",
            "http://dcgm.svc:notaport/metrics",
            "http://[::1",
            "http://[::::]:9400/metrics",
            "http://[1:2:3]:9400/metrics",
            "http://[[::1]:9400/metrics",
            "http://dcgm%2Esvc/metrics",
            r"http://dcgm.svc/metrics\secret",
            "http://Dcgm.Svc:9400/metrics",
            "",
        ):
            with self.subTest(url=url):
                result = render(
                    f"""
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      source: exporter
      exporterUrl: {url!r}
"""
                )
                self.assertNotEqual(0, result.returncode)
                self.assertIn(
                    "must be a nonempty absolute lowercase http(s) URL",
                    result.stderr,
                )

    def test_host_dcgmi_source_requires_loopback_exporter_url(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      exporterUrl: http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            "gpuSkus.h200 dcgmHealth.source=host-dcgmi requires a loopback "
            "dcgmHealth.exporterUrl",
            result.stderr,
        )

    def test_exporter_source_rejects_loopback_urls(self):
        for url in (
            "http://localhost:19400/metrics",
            "http://127.0.0.1:19400/metrics",
            "http://127.0.0.2:9400/metrics",
            "http://[::1]:9400/metrics",
            "http://[0:0:0:0:0:0:0:1]:9400/metrics",
        ):
            with self.subTest(url=url):
                result = render(
                    f"""
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      source: exporter
      exporterUrl: {url!r}
"""
                )
                self.assertNotEqual(0, result.returncode)
                self.assertIn(
                    "gpuSkus.h200 dcgmHealth.source=exporter requires an explicit "
                    "non-loopback dcgmHealth.exporterUrl",
                    result.stderr,
                )

    def test_127_0_0_1_is_an_accepted_loopback_form(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    dcgmHealth:
      exporterUrl: http://127.0.0.1:19400/metrics
""",
            daemonset_only=False,
        )
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn('url: "http://127.0.0.1:19400/metrics"', result.stdout)

    def test_legacy_metrics_collector_scrape_targets_fails_loud(self):
        result = render(
            """
metricsCollector:
  scrapeTargets:
    - name: dcgm-exporter
      url: http://localhost:19400/metrics
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            f"metricsCollector.scrapeTargets was removed in gpu-monitoring "
            f"0.1.7; {LEGACY_KEY_MIGRATION}",
            result.stderr,
        )

    def test_legacy_metrics_collector_dcgm_availability_fails_loud(self):
        result = render(
            """
metricsCollector:
  dcgmAvailability:
    enabled: true
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            f"metricsCollector.dcgmAvailability was removed in gpu-monitoring "
            f"0.1.7; {LEGACY_KEY_MIGRATION}",
            result.stderr,
        )

    def test_legacy_per_profile_scrape_targets_fails_loud(self):
        result = render(
            """
enabledGpuSkus:
  - h200
gpuSkus:
  h200:
    scrapeTargets:
      - name: dcgm-exporter
        url: http://localhost:19400/metrics
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            f"gpuSkus.h200.scrapeTargets was removed in gpu-monitoring 0.1.7; "
            f"{LEGACY_KEY_MIGRATION}",
            result.stderr,
        )

    def test_legacy_per_profile_dcgm_availability_fails_loud(self):
        result = render(
            """
gpuSkus:
  a100:
    dcgmAvailability:
      enabled: false
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            f"gpuSkus.a100.dcgmAvailability was removed in gpu-monitoring "
            f"0.1.7; {LEGACY_KEY_MIGRATION}",
            result.stderr,
        )

    def test_legacy_dcgm_health_required_fails_loud(self):
        result = render(
            """
gpuSkus:
  a100:
    dcgm_health_required: false
"""
        )
        self.assertNotEqual(0, result.returncode)
        self.assertIn(
            f"gpuSkus.a100.dcgm_health_required was removed in gpu-monitoring "
            f"0.1.7; {LEGACY_KEY_MIGRATION}",
            result.stderr,
        )


if __name__ == "__main__":
    unittest.main()
