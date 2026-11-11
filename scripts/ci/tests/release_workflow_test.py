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

    def validate(self, tag="v1.2.3", control=None, ref_type="tag", ref_name=None, status="404"):
        ref_name = ref_name or tag
        ref_prefix = "tags" if ref_type == "tag" else "heads"
        environment = dict(
            os.environ, PATH=str(self.binary) + ":" + os.environ["PATH"],
            DISPATCH_TAG=tag, RELEASE_TAG=tag, GITHUB_SHA=control or self.trusted,
            GITHUB_REF=f"refs/{ref_prefix}/{ref_name}", GITHUB_REF_TYPE=ref_type,
            GITHUB_REF_NAME=ref_name, GH_TOKEN="fixture-token",
            GITHUB_OUTPUT=str(self.output), GITHUB_STEP_SUMMARY=str(self.summary),
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
            "tag_object=" + self.git(self.origin, "rev-parse", "refs/tags/v1.2.3")])
        self.assertIn(self.trusted, self.summary.read_text())

    def test_annotated_off_main_tag_is_rejected(self):
        self.assert_rejected(
            self.validate(tag="v1.2.4", control=self.unreviewed),
            "which is not on main")

    def test_lightweight_main_tag_is_rejected(self):
        self.assert_rejected(self.validate(tag="v1.2.5"), "must be an annotated tag")

    def test_dispatch_revision_must_match_tag(self):
        self.assert_rejected(
            self.validate(tag="v1.2.4"),
            "not the workflow source")

    def test_non_tag_dispatch_is_rejected(self):
        self.assert_rejected(
            self.validate(ref_type="branch", ref_name="main"),
            "Dispatch from the release tag")

    def test_tag_input_must_match_dispatch_ref(self):
        self.assert_rejected(
            self.validate(ref_name="v1.2.4"),
            "tag input must match the dispatch ref")

    def test_unknown_release_read_is_rejected_before_outputs(self):
        self.assert_rejected(self.validate(status="403"),
                             "cannot verify whether GitHub Release v1.2.3 exists (HTTP 403)")

    def test_executable_checkouts_use_the_trusted_workflow_revision(self):
        consumers = set()
        self.assertNotIn("coordinated-artifacts", WORKFLOW["jobs"])
        self.assertNotIn("source_sha", WORKFLOW["jobs"]["validate"]["outputs"])
        for name, job in WORKFLOW["jobs"].items():
            for step in job.get("steps", []):
                if step.get("uses", "").startswith("actions/checkout@"):
                    self.assertIs(step["with"].get("persist-credentials"), False, name)
                    reference = step["with"].get("ref")
                    self.assertEqual(reference, "${{ github.sha }}", name)
                    self.assertNotIn("needs.", reference)
                    self.assertNotIn("inputs.", reference)
                    consumers.add(name)
                    if name != "validate":
                        needs = job.get("needs", [])
                        self.assertIn("validate", [needs] if isinstance(needs, str) else needs)
                        self.assertNotIn("if", job, "payload consumers must not bypass failed validation")
        self.assertEqual(consumers, {"validate", "gates", "build", "publish"})
        for name in ("validate-windows-assets", "smoke-windows"):
            needs = WORKFLOW["jobs"][name]["needs"]
            self.assertIn("build" if name == "validate-windows-assets" else "publish",
                          [needs] if isinstance(needs, str) else needs)
            self.assertNotIn("if", WORKFLOW["jobs"][name])


if __name__ == "__main__":
    unittest.main()
