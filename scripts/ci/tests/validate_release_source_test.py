#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import validate_release_source


def run(directory, *arguments):
    subprocess.run(arguments, cwd=directory, check=True, capture_output=True)


class ValidateReleaseSourceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name)
        self.remote = root / "remote.git"
        self.author = root / "author"
        self.source = root / "source"
        run(root, "git", "init", "--bare", self.remote)
        run(root, "git", "init", "-b", "main", self.author)
        run(self.author, "git", "config", "user.name", "Release Test")
        run(self.author, "git", "config", "user.email", "release@example.invalid")
        run(self.author, "git", "config", "tag.gpgSign", "false")
        (self.author / "file").write_text("main\n")
        run(self.author, "git", "add", "file")
        run(self.author, "git", "commit", "-m", "main")
        run(self.author, "git", "remote", "add", "origin", self.remote)
        run(self.author, "git", "push", "-u", "origin", "main")
        run(self.author, "git", "tag", "-a", "v1.2.3", "-m", "v1.2.3")
        run(self.author, "git", "push", "origin", "v1.2.3")
        run(root, "git", "clone", self.remote, self.source)
        run(self.source, "git", "checkout", "v1.2.3")
        self.commit = subprocess.run(
            ("git", "rev-parse", "HEAD"), cwd=self.source, check=True,
            capture_output=True, text=True).stdout.strip()

    def test_accepts_exact_annotated_main_tag(self):
        self.assertEqual(
            validate_release_source.validate(self.source, "v1.2.3", self.commit),
            self.commit,
        )

    def test_rejects_lightweight_mismatched_and_off_main_tags(self):
        run(self.author, "git", "tag", "v1.2.4")
        run(self.author, "git", "push", "origin", "v1.2.4")
        with self.assertRaisesRegex(ValueError, "annotated"):
            validate_release_source.validate(self.source, "v1.2.4", self.commit)
        with self.assertRaisesRegex(ValueError, "not " + "b" * 40):
            validate_release_source.validate(self.source, "v1.2.3", "b" * 40)

        run(self.author, "git", "checkout", "--orphan", "off-main")
        run(self.author, "git", "rm", "-f", "file")
        (self.author / "other").write_text("other\n")
        run(self.author, "git", "add", "other")
        run(self.author, "git", "commit", "-m", "other")
        run(self.author, "git", "tag", "-a", "v1.2.5", "-m", "v1.2.5")
        run(self.author, "git", "push", "origin", "v1.2.5")
        off_main = subprocess.run(
            ("git", "rev-parse", "HEAD"), cwd=self.author, check=True,
            capture_output=True, text=True).stdout.strip()
        run(self.source, "git", "fetch", "origin", "v1.2.5")
        run(self.source, "git", "checkout", off_main)
        with self.assertRaisesRegex(ValueError, "not on main"):
            validate_release_source.validate(self.source, "v1.2.5", off_main)


if __name__ == "__main__":
    unittest.main()
