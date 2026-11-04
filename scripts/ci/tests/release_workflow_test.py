#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/release-tau.yaml").read_text())


class ReleaseWorkflowTrustTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.origin = self.directory / "origin"
        self.origin.mkdir()
        self.git(self.origin, "init", "-b", "main")
        for key, value in (("user.name", "Release fixture"),
                           ("user.email", "fixture@example.invalid"),
                           ("commit.gpgSign", "false"), ("tag.gpgSign", "false"),
                           ("core.hooksPath", "/dev/null")):
            self.git(self.origin, "config", key, value)
        notes = self.origin / "cli/releases"
        notes.mkdir(parents=True)
        for tag in ("v1.2.3", "v1.2.4", "v1.2.5"):
            (notes / f"{tag}.md").write_text("Synthetic release notes\n")
        self.git(self.origin, "add", ".")
        self.git(self.origin, "commit", "-m", "Reviewed main fixture")
        self.trusted = self.git(self.origin, "rev-parse", "HEAD")
        self.git(self.origin, "tag", "-a", "v1.2.3", "-m", "Reviewed fixture")
        self.git(self.origin, "tag", "--no-sign", "v1.2.5")
        self.git(self.origin, "checkout", "-b", "unreviewed")
        (self.origin / "unreviewed").write_text("Unreviewed source\n")
        self.git(self.origin, "add", ".")
        self.git(self.origin, "commit", "-m", "Unreviewed branch fixture")
        self.unreviewed = self.git(self.origin, "rev-parse", "HEAD")
        self.git(self.origin, "tag", "-a", "v1.2.4", "-m", "Unreviewed fixture")
        self.git(self.origin, "checkout", "main")
        self.checkout = self.directory / "checkout"
        self.git(self.directory, "clone", "--quiet", str(self.origin), str(self.checkout))
        self.git(self.checkout, "checkout", "--detach", self.trusted)
        self.binary = self.directory / "bin"
        self.binary.mkdir()
        self.curl = self.binary / "curl"
        self.curl.write_text('#!/bin/sh\nprintf "%s" "$FIXTURE_RELEASE_STATUS"\n')
        self.curl.chmod(0o755)
        self.script = next(step["run"] for step in WORKFLOW["jobs"]["validate"]["steps"]
                           if step.get("id") == "release")
        self.output = self.directory / "output"
        self.summary = self.directory / "summary"

    def git(self, directory, *arguments):
        return subprocess.check_output(
            ["git", "-C", str(directory), *arguments], text=True,
            stderr=subprocess.DEVNULL).strip()

    def validate(self, tag="v1.2.3", control=None, ref="refs/heads/main", status="404"):
        environment = dict(
            os.environ, PATH=str(self.binary) + ":" + os.environ["PATH"],
            DISPATCH_TAG=tag, RELEASE_TAG=tag, GITHUB_SHA=control or self.trusted,
            GITHUB_REF=ref, GITHUB_OUTPUT=str(self.output), GITHUB_STEP_SUMMARY=str(self.summary),
            RUNNER_TEMP=str(self.directory), GITHUB_API_URL="https://example.invalid",
            GITHUB_REPOSITORY="example/project", FIXTURE_RELEASE_STATUS=status)
        return subprocess.run(["bash", "-c", self.script], cwd=self.checkout, env=environment,
                              text=True, capture_output=True)

    def assert_rejected(self, result, message):
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(message, result.stderr)
        self.assertFalse(self.output.exists(), "rejected source must not emit validated outputs")
        self.assertFalse(self.summary.exists())

    def test_annotated_main_tag_emits_exact_immutable_source_and_tag_object(self):
        result = self.validate()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_text().splitlines(), [
            f"source_sha={self.trusted}",
            "tag_object=" + self.git(self.origin, "rev-parse", "refs/tags/v1.2.3")])
        self.assertIn(self.trusted, self.summary.read_text())

    def test_annotated_off_main_tag_is_rejected(self):
        self.assert_rejected(self.validate(tag="v1.2.4"), "which is not on main")

    def test_lightweight_main_tag_is_rejected(self):
        self.assert_rejected(self.validate(tag="v1.2.5"), "must be an annotated tag")

    def test_unreviewed_orchestration_revision_is_rejected(self):
        self.assert_rejected(self.validate(control=self.unreviewed),
                             "Orchestration revision is not on main")

    def test_non_main_dispatch_is_rejected(self):
        self.assert_rejected(self.validate(ref="refs/heads/unreviewed"),
                             "Dispatch reviewed release orchestration from main")

    def test_unknown_release_read_is_rejected_before_outputs(self):
        self.assert_rejected(self.validate(status="403"),
                             "cannot verify whether GitHub Release v1.2.3 exists (HTTP 403)")

    def test_payload_execution_requires_successful_validation_and_no_checkout_credentials(self):
        consumers = set()
        for name, job in WORKFLOW["jobs"].items():
            for step in job.get("steps", []):
                if step.get("uses", "").startswith("actions/checkout@"):
                    self.assertIs(step["with"].get("persist-credentials"), False, name)
                    if step["with"].get("ref") == "${{ needs.validate.outputs.source_sha }}":
                        consumers.add(name)
                        needs = job.get("needs", [])
                        self.assertIn("validate", [needs] if isinstance(needs, str) else needs)
                        self.assertNotIn("if", job, "payload consumers must not bypass failed validation")
        self.assertEqual(consumers, {"gates", "build", "coordinated-artifacts", "publish", "smoke"})
        for name in ("validate-windows-assets", "smoke-windows"):
            needs = WORKFLOW["jobs"][name]["needs"]
            self.assertIn("build" if name == "validate-windows-assets" else "publish",
                          [needs] if isinstance(needs, str) else needs)
            self.assertNotIn("if", WORKFLOW["jobs"][name])


if __name__ == "__main__":
    unittest.main()
