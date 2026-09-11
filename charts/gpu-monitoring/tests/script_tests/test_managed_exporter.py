# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import contextlib
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "configurator",
    Path(__file__).resolve().parents[2] / "operations/configure-managed-dcgm-exporter.py",
)
configurator = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(configurator)

BASE = "DCGM_FI_DEV_GPU_UTIL, gauge, Existing definition.\n"
EXTRA = "DCGM_FI_DEV_GPU_UTIL, counter, Must not replace.\nDCGM_FI_DEV_ECC_DBE_VOL_TOTAL, counter, ECC.\n"


class ConfigurationTest(unittest.TestCase):
    def setUp(self):
        self.context = contextlib.ExitStack()
        self.addCleanup(self.context.close)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name).resolve()
        self.default = root / "default.csv"
        self.default.write_text(BASE)
        self.owned = root / "owned"
        self.dropin = root / "90-taugrid-metrics.conf"
        for name, value in {
            "DEFAULT": self.default, "DIRECTORY": self.owned,
            "STATE": self.owned / "state.json", "DROPIN": self.dropin,
        }.items():
            self.context.enter_context(patch.object(configurator, name, value))
        self.context.enter_context(patch.object(configurator, "exporter_argv", return_value=configurator.BASE_ARGV))
        self.context.enter_context(patch.object(configurator, "protected_files", return_value={"aks": "original"}))
        self.context.enter_context(patch.object(configurator, "service_pid", return_value=123))
        self.context.enter_context(patch.object(configurator, "gpu_uuids", return_value=["GPU-abc"]))
        self.verify = self.context.enter_context(patch.object(configurator, "verify"))
        self.restart = self.context.enter_context(patch.object(configurator, "restart_exporter"))
        self.context.enter_context(patch.object(configurator, "run"))

    def test_preserves_every_original_definition(self):
        merged = configurator.merge_csv(BASE, EXTRA)
        self.assertEqual(configurator.fields(merged)["DCGM_FI_DEV_GPU_UTIL"], BASE.strip())
        self.assertEqual(len(configurator.fields(merged)), 2)

    def test_rejects_invalid_duplicate_and_empty_input(self):
        for text in ("", "garbage", EXTRA + EXTRA, "DCGM_FI_BAD, invalid, description"):
            with self.subTest(text=text), self.assertRaises(ValueError):
                configurator.merge_csv(BASE, text)

    def test_plan_never_writes_or_restarts(self):
        result = configurator.configure("plan", EXTRA)
        self.assertEqual(result["added_fields"], 1)
        self.assertFalse(self.owned.exists())
        self.restart.assert_not_called()

    def test_apply_idempotence_and_rollback(self):
        first = configurator.configure("apply", EXTRA)
        self.assertTrue(first["changed"])
        self.assertIn("--address :19400", self.dropin.read_text())
        self.assertEqual(self.default.read_text(), BASE)
        self.assertFalse(configurator.configure("apply", EXTRA)["changed"])
        self.assertEqual(self.restart.call_count, 1)
        result = configurator.configure("rollback", None)
        self.assertTrue(result["rolled_back"])
        self.assertFalse(self.dropin.exists())
        self.assertEqual(list(self.owned.iterdir()), [])
        self.assertEqual(self.default.read_text(), BASE)

    def test_failed_restart_restores_original_configuration(self):
        self.restart.side_effect = [RuntimeError("failed exporter"), None]
        with self.assertRaisesRegex(RuntimeError, "original AKS exporter restored"):
            configurator.configure("apply", EXTRA)
        self.assertFalse(self.dropin.exists())
        self.assertEqual(list(self.owned.iterdir()), [])
        self.assertEqual(self.restart.call_count, 2)

    def test_foreign_dropin_and_symlink_refused(self):
        self.dropin.write_text("Foreign owner")
        with self.assertRaises(RuntimeError):
            configurator.configure("apply", EXTRA)
        self.dropin.unlink()
        self.dropin.symlink_to(self.default)
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            configurator.configure("apply", EXTRA)
        self.restart.assert_not_called()

    def test_modified_owned_file_and_protected_state_refuse_rollback(self):
        configurator.configure("apply", EXTRA)
        original = self.dropin.read_text()
        self.dropin.write_text(original + "# changed\n")
        with self.assertRaisesRegex(RuntimeError, "Owned configuration changed"):
            configurator.configure("rollback", None)
        self.dropin.write_text(original)
        with patch.object(configurator, "protected_files", return_value={"aks": "changed"}):
            with self.assertRaisesRegex(RuntimeError, "Protected host state changed"):
                configurator.configure("rollback", None)
        self.assertTrue(self.dropin.exists())

    def test_incomplete_file_write_leaves_no_partial_configuration(self):
        with patch.object(configurator.os, "fsync", side_effect=OSError("disk error")):
            with self.assertRaisesRegex(OSError, "disk error"):
                configurator.configure("apply", EXTRA)
        self.assertFalse(self.dropin.exists())
        self.assertEqual(list(self.owned.iterdir()), [])
        self.restart.assert_not_called()


if __name__ == "__main__":
    unittest.main()
