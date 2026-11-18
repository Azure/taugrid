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


def load_checker(name):
    spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


CONTENT = load_checker("check_content")
PARITY = load_checker("check_wiki_parity")


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
        self.content = self.root / "content"
        self.content.mkdir()
        (self.root / "data").mkdir()
        self.page = self.content / "capability.md"
        self.manifest = self.root / "data/wiki-parity.toml"
        self.write_page()
        self.write_manifest()

    def write_page(self, reviewed="2026-07-16", status="alpha"):
        self.page.write_text(
            f'{{{{< maturity status="{status}" reviewed="{reviewed}" >}}}}\n',
            encoding="utf-8",
        )

    def write_manifest(self, reviewed="2026-07-16", extra=""):
        self.manifest.write_text(
            '[[pairs]]\nid = "capability"\nsite = "content/capability.md"\n'
            'wiki = "https://github.com/Azure/taugrid/wiki/Capability"\n'
            'owner = "taugrid"\nmaturity = "alpha"\n'
            f'reviewed = "{reviewed}"\n{extra}',
            encoding="utf-8",
        )

    def run_checker(self, checker, *args):
        output = io.StringIO()
        with (
            mock.patch.object(sys, "argv", [checker.__name__, *map(str, args)]),
            mock.patch.object(checker.datetime, "date", AuditDate),
            contextlib.redirect_stdout(output),
            contextlib.redirect_stderr(output),
        ):
            result = checker.main()
        return result, output.getvalue()

    def check_content(self, *args):
        return self.run_checker(CONTENT, self.content, *args)

    def check_parity(self, *args):
        return self.run_checker(PARITY, "--manifest", self.manifest, *args)

    def test_expired_reviews_are_only_blocking_in_strict_audit(self):
        for check in (self.check_content, self.check_parity):
            with self.subTest(check=check.__name__):
                result, output = check()
                self.assertEqual(result, 1)
                self.assertIn("125 days old", output)
                result, output = check("--skip-review-age")
                self.assertEqual(result, 0, output)

    def test_review_age_boundary(self):
        for check, limit in ((self.check_content, 120), (self.check_parity, 90)):
            for age in (limit, limit + 1):
                reviewed = (AuditDate.today() - datetime.timedelta(days=age)).isoformat()
                self.write_page(reviewed)
                self.write_manifest(reviewed)
                with self.subTest(check=check.__name__, age=age):
                    result, output = check()
                    self.assertEqual(result, int(age > limit), output)

    def test_independent_site_review_is_advisory_not_joint_review(self):
        self.write_page("2026-11-18")
        self.write_manifest("2026-11-17")
        before = self.manifest.read_bytes()
        for args in ((), ("--skip-review-age",)):
            result, output = self.check_parity(*args)
            self.assertEqual(result, 0, output)
            self.assertIn("Advisory: capability", output)
            self.assertIn("wiki counterpart needs an update", output)
        self.assertEqual(self.manifest.read_bytes(), before)

    def test_new_site_review_does_not_refresh_expired_joint_review(self):
        self.write_page("2026-11-18")
        result, output = self.check_parity()
        self.assertEqual(result, 1)
        self.assertIn("site/wiki review is 125 days old", output)
        self.assertEqual(self.check_parity("--skip-review-age")[0], 0)

    def test_invalid_and_future_site_dates_always_fail(self):
        for date in ("", "not-a-date", "20261118", "2026-02-30", "2026-11-19"):
            self.write_page(date)
            for check in (self.check_content, self.check_parity):
                with self.subTest(date=date, check=check.__name__):
                    result, output = check("--skip-review-age")
                    self.assertEqual(result, 1, output)
                    self.assertIn("review date", output)

    def test_invalid_and_future_joint_dates_always_fail(self):
        for date in ("not-a-date", "2026-02-30", "2026-11-19"):
            self.write_manifest(date)
            result, output = self.check_parity("--skip-review-age")
            self.assertEqual(result, 1, output)
            self.assertIn("review date", output)

    def test_maturity_checks_still_block(self):
        self.write_page(status="unsupported")
        result, output = self.check_content("--skip-review-age")
        self.assertEqual(result, 1)
        self.assertIn("unsupported maturity", output)
        self.write_page(status="ga")
        result, output = self.check_parity("--skip-review-age")
        self.assertEqual(result, 1)
        self.assertIn("does not match site maturity", output)

    def test_section_maturity_still_blocks(self):
        self.page.rename(self.content / "_index.md")
        result, output = self.check_content("--skip-review-age")
        self.assertEqual(result, 1)
        self.assertIn("section indexes must not declare", output)

    def test_missing_page_owner_and_invalid_wiki_still_block(self):
        self.page.unlink()
        self.manifest.write_text(
            self.manifest.read_text().replace('owner = "taugrid"\n', "").replace(
                "https://github.com/Azure/taugrid/wiki/Capability",
                "https://example.invalid/Capability",
            ),
            encoding="utf-8",
        )
        result, output = self.check_parity("--skip-review-age")
        self.assertEqual(result, 1)
        for message in ("missing site page", "missing owner", "invalid wiki URL"):
            self.assertIn(message, output)

    def test_site_only_freshness_and_structure(self):
        self.write_manifest(
            "2026-11-18",
            extra=(
                '\n[[site_only]]\nid = "site-only"\nsite = "content/capability.md"\n'
                'owner = "taugrid"\nreason = "Versioned reference"\n'
                'reviewed = "2026-07-16"\n'
            ),
        )
        result, output = self.check_parity()
        self.assertEqual(result, 1)
        self.assertIn("site-only rationale is 125 days old", output)
        self.assertEqual(self.check_parity("--skip-review-age")[0], 0)
        self.manifest.write_text(
            self.manifest.read_text().replace('reason = "Versioned reference"\n', ""),
            encoding="utf-8",
        )
        result, output = self.check_parity("--skip-review-age")
        self.assertEqual(result, 1)
        self.assertIn("missing site-only rationale", output)

    def test_strict_make_audit_runs_both_checks_despite_skip_overrides(self):
        self.write_page("2000-01-01")
        self.write_manifest("2000-01-01")
        docs = self.content / "en/docs"
        docs.mkdir(parents=True)
        (docs / "capability.md").write_bytes(self.page.read_bytes())
        scripts = self.root / "scripts"
        scripts.mkdir()
        for name in ("check_content.py", "check_wiki_parity.py"):
            (scripts / name).symlink_to(SCRIPTS / name)
        result = subprocess.run(
            [
                "make", "-s", "-f", str(SCRIPTS.parent / "Makefile"),
                "check-review-age",
                "CONTENT_ARGS=--skip-review-age",
                "WIKI_PARITY_ARGS=--skip-review-age",
            ],
            cwd=self.root,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("capability.md: review is", result.stderr)
        self.assertIn("capability: site/wiki review is", result.stderr)


if __name__ == "__main__":
    unittest.main()
