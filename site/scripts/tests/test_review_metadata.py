# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import contextlib
import datetime
import importlib.util
import io
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SCRIPTS = pathlib.Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "check_content", SCRIPTS / "check_content.py"
)
CONTENT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CONTENT)


class AuditDate(datetime.date):
    @classmethod
    def today(cls):
        return cls(2026, 11, 18)


class ReviewMetadataTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(
            prefix=".docs-review-test-", dir=pathlib.Path.cwd()
        )
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.content = self.root / "content/en/docs"
        self.content.mkdir(parents=True)
        self.page = self.content / "capability.md"
        self.write_page()

    def write_page(self, reviewed="2026-07-16", status="alpha"):
        self.page.write_text(
            f'{{{{< maturity status="{status}" reviewed="{reviewed}" >}}}}\n',
            encoding="utf-8",
        )

    def check_content(self, *args):
        output = io.StringIO()
        with (
            mock.patch.object(sys, "argv", ["check_content", str(self.content), *args]),
            mock.patch.object(CONTENT.datetime, "date", AuditDate),
            contextlib.redirect_stdout(output),
            contextlib.redirect_stderr(output),
        ):
            result = CONTENT.main()
        return result, output.getvalue()

    def test_expired_reviews_are_only_blocking_in_strict_audit(self):
        result, output = self.check_content()
        self.assertEqual(result, 1)
        self.assertIn("125 days old", output)
        result, output = self.check_content("--skip-review-age")
        self.assertEqual(result, 0, output)

    def test_review_age_boundary(self):
        for age in (120, 121):
            reviewed = (AuditDate.today() - datetime.timedelta(days=age)).isoformat()
            self.write_page(reviewed)
            with self.subTest(age=age):
                result, output = self.check_content()
                self.assertEqual(result, int(age > 120), output)

    def test_invalid_and_future_dates_always_fail(self):
        for date in ("", "not-a-date", "20261118", "2026-02-30", "2026-11-19"):
            self.write_page(date)
            for args in ((), ("--skip-review-age",)):
                with self.subTest(date=date, args=args):
                    result, output = self.check_content(*args)
                    self.assertEqual(result, 1, output)
                    self.assertIn("review date", output)

    def test_maturity_checks_still_block(self):
        self.write_page(status="unsupported")
        result, output = self.check_content("--skip-review-age")
        self.assertEqual(result, 1)
        self.assertIn("unsupported maturity", output)

    def test_section_maturity_still_blocks(self):
        self.page.rename(self.content / "_index.md")
        result, output = self.check_content("--skip-review-age")
        self.assertEqual(result, 1)
        self.assertIn("section indexes must not declare", output)

    def test_strict_make_audit_ignores_skip_override(self):
        self.write_page("2000-01-01")
        scripts = self.root / "scripts"
        scripts.mkdir()
        (scripts / "check_content.py").symlink_to(SCRIPTS / "check_content.py")
        result = subprocess.run(
            [
                "make", "-s", "-f", str(SCRIPTS.parent / "Makefile"),
                "check-review-age", "CONTENT_ARGS=--skip-review-age",
            ],
            cwd=self.root,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("capability.md: review is", result.stderr)


if __name__ == "__main__":
    unittest.main()
