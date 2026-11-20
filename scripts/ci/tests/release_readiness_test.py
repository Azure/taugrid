#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
import importlib.util
import importlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

CI = Path(__file__).resolve().parents[1]
ROOT = CI.parents[1]
sys.path.insert(0, str(CI))
artifacts = importlib.import_module("release_artifacts")

SPEC = importlib.util.spec_from_file_location(
    "readiness", CI / "verify-release-readiness.py"
)
ready = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ready)
SHA = "a" * 40
TAG_OBJECT = "b" * 40
TREE = "c" * 40


def release():
    return {
        "commit": SHA,
        "version": "1.2.3",
        "charts": {
            name: {
                "version": "1.2.3" if name in artifacts.PRODUCT_CHARTS else "0.1.0",
                "source_tree": TREE,
            }
            for name in artifacts.CHARTS
        },
    }


def chart_package(name, metadata=None):
    source = release()
    value = metadata or {
        "name": name,
        "version": source["charts"][name]["version"],
        "appVersion": "1.2.3",
        "annotations": {artifacts.REVISION: SHA, artifacts.SOURCE_TREE: TREE},
    }
    lines = [
        f"name: {value['name']}",
        f"version: {value['version']}",
        f"appVersion: {value['appVersion']}",
    ]
    if "annotations" in value:
        lines.append("annotations:")
        lines.extend(
            f'  {key}: "{annotation}"'
            for key, annotation in value["annotations"].items()
        )
    data = ("\n".join(lines) + "\n").encode()
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as archive:
        entry = tarfile.TarInfo(f"{name}/Chart.yaml")
        entry.size = len(data)
        archive.addfile(entry, io.BytesIO(data))
    return output.getvalue()


def workflow_run():
    return {
        "repository": {"full_name": "Azure/taugrid"},
        "head_repository": {"full_name": "Azure/taugrid"},
        "path": ".github/workflows/release-tau.yaml",
        "event": "workflow_dispatch",
        "head_branch": "v1.2.3",
        "head_sha": SHA,
        "display_title": "Release TauGrid v1.2.3",
        "status": "completed",
        "conclusion": "success",
    }


class FakeGitHub(ready.GitHub):
    def __init__(self):
        super().__init__(None, "Azure/taugrid", None)
        self.run = workflow_run()
        self.ref = {"type": "tag", "sha": TAG_OBJECT}
        self.object = {"tag": "v1.2.3", "object": {"type": "commit", "sha": SHA}}
        self.published = {"tag_name": "v1.2.3", "draft": False, "prerelease": False}

    def get(self, path):
        if path.startswith("git/ref/"):
            return {"object": self.ref}
        if path.startswith("git/tags/"):
            return self.object
        if path.startswith("releases/"):
            return self.published
        if path.startswith("actions/workflows/"):
            return {"workflow_runs": [self.run]}
        return self.run


class GateTests(unittest.TestCase):
    def test_automatic_and_manual_success(self):
        for tag, run_id in (("", "123"), ("v1.2.3", "")):
            self.assertEqual(
                FakeGitHub().resolve(tag, run_id), ("v1.2.3", (TAG_OBJECT, SHA))
            )

    def test_only_stable_annotated_direct_commit_tags(self):
        github = FakeGitHub()
        for tag in ("v1.2.3-rc.1", "v1.2.3+build", "1.2.3", "v01.2.3", "../main"):
            with self.subTest(tag=tag), self.assertRaises(artifacts.ReleaseError):
                github.tag(tag)
        github.ref["type"] = "commit"
        with self.assertRaisesRegex(artifacts.ReleaseError, "annotated"):
            github.tag("v1.2.3")
        github.ref["type"] = "tag"
        github.object["object"]["type"] = "tag"
        with self.assertRaisesRegex(artifacts.ReleaseError, "directly"):
            github.tag("v1.2.3")

    def test_untrusted_or_failed_workflow_cannot_open_gate(self):
        changes = {
            "path": ".github/workflows/other.yaml",
            "head_sha": "d" * 40,
            "head_branch": "main",
            "status": "in_progress",
            "conclusion": "failure",
            "event": "push",
            "repository": {"full_name": "fork/taugrid"},
            "head_repository": {"full_name": "fork/taugrid"},
            "display_title": "untrusted",
        }
        for field, value in changes.items():
            with self.subTest(field=field):
                github = FakeGitHub()
                github.run[field] = value
                with self.assertRaises(artifacts.ReleaseError):
                    github.resolve("", "123")

    def test_manual_requires_successful_cli_gate(self):
        github = FakeGitHub()
        github.run["conclusion"] = "failure"
        with self.assertRaisesRegex(artifacts.ReleaseError, "no successful"):
            github.resolve("v1.2.3", "")

    def test_published_stable_release_required(self):
        for field in ("draft", "prerelease"):
            github = FakeGitHub()
            github.published[field] = True
            with self.subTest(field=field), self.assertRaises(artifacts.ReleaseError):
                github.resolve("v1.2.3", "")

    def test_moved_tag_and_requested_source_are_rejected(self):
        github = FakeGitHub()
        github.object["object"]["sha"] = "d" * 40
        with self.assertRaises(artifacts.ReleaseError):
            github.resolve("", "123")
        with patch.object(
            ready.GitHub, "resolve", return_value=("v1.2.3", (TAG_OBJECT, SHA))
        ):
            with patch.object(
                sys, "argv", ["verify", "--tag", "v1.2.3", "--commit", "d" * 40]
            ):
                with self.assertRaisesRegex(artifacts.ReleaseError, "source commit"):
                    ready.main()


class PollTests(unittest.TestCase):
    class Registry:
        def __init__(self, missing=()):
            self.missing = missing
            self.calls = []

        def image(self, name, source):
            self.calls.append(("image", name))
            if name in self.missing:
                raise ready.MissingArtifact(name)

        def chart(self, name, source):
            self.calls.append(("chart", name))
            if name in self.missing:
                raise ready.MissingArtifact(name)

    def test_all_nine_artifacts_required(self):
        registry = self.Registry()
        identity = []
        ready.wait_ready(
            registry, release(), 10, 1, lambda: identity.append(True), clock=lambda: 0
        )
        self.assertEqual(len(registry.calls), 9)
        self.assertEqual(identity, [True])

    def test_partial_release_times_out_with_missing_set(self):
        registry = self.Registry(("tau", "adx-mon"))
        with self.assertRaisesRegex(artifacts.ReleaseError, "image/tau, chart/adx-mon"):
            ready.wait_ready(registry, release(), 0, 1, lambda: None, clock=lambda: 0)

    def test_missing_artifact_can_arrive_within_bound(self):
        registry = self.Registry(("tau",))
        waits = []

        def sleep(seconds):
            waits.append(seconds)
            registry.missing = ()

        ready.wait_ready(
            registry, release(), 10, 1, lambda: None, sleep=sleep, clock=lambda: 0
        )
        self.assertEqual(waits, [1])
        self.assertEqual(len(registry.calls), 18)

    def test_check_once_reports_only_missing_artifacts_as_incomplete(self):
        registry = self.Registry(("tau", "adx-mon"))
        self.assertEqual(
            ready.check_ready(registry, release(), lambda: None),
            ["image/tau", "chart/adx-mon"],
        )
        self.assertEqual(len(registry.calls), 9)

    def test_errors_and_moved_tags_are_not_retried(self):
        registry = self.Registry()
        with patch.object(
            registry, "image", side_effect=artifacts.ReleaseError("HTTP 403")
        ):
            with self.assertRaisesRegex(artifacts.ReleaseError, "HTTP 403"):
                ready.wait_ready(
                    registry, release(), 10, 1, lambda: None, clock=lambda: 0
                )

        def moved():
            raise artifacts.ReleaseError("tag moved")

        with self.assertRaisesRegex(artifacts.ReleaseError, "tag moved"):
            ready.wait_ready(registry, release(), 10, 1, moved, clock=lambda: 0)

    def test_http_missing_auth_network_and_deadline_are_distinct(self):
        http = ready.HTTP(100)
        url = "https://mcr.microsoft.com/v2/test"
        with patch.object(ready.time, "monotonic", return_value=0):
            for code in (404, 401, 403, 429, 500):
                error = urllib.error.HTTPError(url, code, "failure", None, None)
                expected = (
                    ready.MissingArtifact if code == 404 else artifacts.ReleaseError
                )
                with patch.object(ready.urllib.request, "urlopen", side_effect=error):
                    with self.subTest(code=code), self.assertRaises(expected):
                        http.get(url, missing=True)
            with patch.object(
                ready.urllib.request,
                "urlopen",
                side_effect=urllib.error.URLError("offline"),
            ):
                with self.assertRaisesRegex(artifacts.ReleaseError, "network error"):
                    http.get(url, missing=True)
        with patch.object(ready.time, "monotonic", return_value=101):
            with self.assertRaisesRegex(artifacts.ReleaseError, "deadline"):
                http.get(url)


class ArtifactTests(unittest.TestCase):
    def test_bounded_chart_parser_preserves_unrelated_yaml(self):
        source = b"""apiVersion: v2
name: taugrid
description: |
  Multi-line description.
version: 1.2.3
appVersion: "1.2.3"
dependencies:
  - name: dependency
    version: 4.5.6
"""
        stamped = artifacts.stamp_chart(source, "taugrid", release())
        self.assertIn(b"description: |\n  Multi-line description.", stamped)
        self.assertIn(b"dependencies:\n  - name: dependency", stamped)
        chart = artifacts.chart_metadata(stamped, "taugrid")
        self.assertEqual(chart["name"], "taugrid")
        self.assertEqual(chart["annotations"][artifacts.REVISION], SHA)
        self.assertEqual(chart["annotations"][artifacts.SOURCE_TREE], TREE)

    def test_bounded_chart_parser_rejects_duplicate_identity(self):
        with self.assertRaisesRegex(artifacts.ReleaseError, "duplicate name"):
            artifacts.chart_metadata(
                b"name: taugrid\nname: other\nversion: 1.2.3\n",
                "taugrid",
            )

    def test_product_revision_and_independent_reuse(self):
        for name in artifacts.CHARTS:
            value = artifacts.archive_chart(chart_package(name), name)
            value["annotations"][artifacts.REVISION] = "d" * 40
            data = chart_package(name, value)
            if name in artifacts.PRODUCT_CHARTS:
                with self.assertRaisesRegex(artifacts.ReleaseError, "provenance"):
                    artifacts.verify_chart(data, name, release())
            else:
                artifacts.verify_chart(data, name, release())
            value["annotations"][artifacts.SOURCE_TREE] = "d" * 40
            with self.assertRaisesRegex(artifacts.ReleaseError, "source-tree"):
                artifacts.verify_chart(chart_package(name, value), name, release())
            value.pop("annotations")
            with self.assertRaisesRegex(artifacts.ReleaseError, "source-tree"):
                artifacts.verify_chart(chart_package(name, value), name, release())

    def test_image_requires_revision_and_both_platforms(self):
        registry = ready.Registry(None)
        manifest = {
            "annotations": {artifacts.REVISION: SHA},
            "manifests": [
                {"platform": {"os": "linux", "architecture": arch}}
                for arch in ("amd64", "arm64")
            ],
        }
        with patch.object(registry, "manifest", return_value=manifest):
            registry.image("tau", release())
            manifest["manifests"].pop()
            with self.assertRaisesRegex(artifacts.ReleaseError, "linux/amd64"):
                registry.image("tau", release())
            manifest["annotations"] = {}
            with self.assertRaisesRegex(artifacts.ReleaseError, "revision"):
                registry.image("tau", release())

    def test_registry_chart_layer_digest_and_provenance(self):
        data = chart_package("taugrid")
        digest = "sha256:" + hashlib.sha256(data).hexdigest()

        class FakeHTTP:
            def get(self, url, **kwargs):
                if "/manifests/" in url:
                    return json.dumps(
                        {
                            "layers": [
                                {
                                    "mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
                                    "digest": digest,
                                }
                            ]
                        }
                    ).encode()
                return data

        registry = ready.Registry(FakeHTTP())
        registry.chart("taugrid", release())
        data += b"corruption"
        with self.assertRaisesRegex(artifacts.ReleaseError, "digest mismatch"):
            registry.chart("taugrid", release())

    def test_seal_and_manifest_reject_changed_bytes_or_release(self):
        with tempfile.TemporaryDirectory() as work:
            directory = Path(work)
            for name, chart in release()["charts"].items():
                (directory / f"{name}-{chart['version']}.tgz").write_bytes(
                    chart_package(name)
                )
            artifacts.seal(directory, release())
            artifacts.verify_directory(directory, release())
            other = release()
            other["commit"] = "d" * 40
            with self.assertRaisesRegex(artifacts.ReleaseError, "manifest"):
                artifacts.verify_directory(directory, other)
            (directory / "taugrid-1.2.3.tgz").write_bytes(b"changed")
            with self.assertRaisesRegex(artifacts.ReleaseError, "bytes changed"):
                artifacts.verify_directory(directory, release())

    def test_source_tree_is_tagged_content_not_packaging_noise(self):
        with tempfile.TemporaryDirectory() as work:
            previous = os.getcwd()
            try:
                os.chdir(work)
                subprocess.run(["git", "init", "--quiet"], check=True)
                for name, chart in release()["charts"].items():
                    directory = Path("charts") / name
                    directory.mkdir(parents=True)
                    (directory / "Chart.yaml").write_text(
                        "\n".join(
                            (
                                f"name: {name}",
                                f"version: {chart['version']}",
                                'appVersion: "1.2.3"',
                                "",
                            )
                        )
                    )
                subprocess.run(["git", "add", "charts"], check=True)
                subprocess.run(
                    [
                        "git",
                        "-c",
                        "user.name=Test",
                        "-c",
                        "user.email=test@example.invalid",
                        "commit",
                        "--quiet",
                        "-m",
                        "fixture",
                    ],
                    check=True,
                )
                commit = artifacts.git("rev-parse", "HEAD")
                source = artifacts.metadata(commit, "1.2.3")
                (Path("charts/adx-mon") / "generated.tgz").write_bytes(b"noise")
                self.assertEqual(artifacts.metadata(commit, "1.2.3"), source)
                (Path("charts/adx-mon") / "values.yaml").write_text("changed: true\n")
                subprocess.run(["git", "add", "charts/adx-mon/values.yaml"], check=True)
                subprocess.run(
                    [
                        "git",
                        "-c",
                        "user.name=Test",
                        "-c",
                        "user.email=test@example.invalid",
                        "commit",
                        "--quiet",
                        "-m",
                        "change",
                    ],
                    check=True,
                )
                updated = artifacts.metadata(
                    artifacts.git("rev-parse", "HEAD"), "1.2.3"
                )
                self.assertNotEqual(
                    updated["charts"]["adx-mon"], source["charts"]["adx-mon"]
                )
                with self.assertRaises(artifacts.ReleaseError):
                    artifacts.metadata(commit, "1.2.4")
            finally:
                os.chdir(previous)


class WiringTests(unittest.TestCase):
    def test_check_once_exit_contract(self):
        with patch.object(
            ready, "main", side_effect=ready.IncompleteRelease("missing")
        ):
            self.assertEqual(ready.entrypoint(), 3)
        with patch.object(
            ready, "main", side_effect=artifacts.ReleaseError("invalid")
        ):
            self.assertEqual(ready.entrypoint(), 1)
        with patch.object(ready, "main"):
            self.assertEqual(ready.entrypoint(), 0)

    def test_check_once_wiring_does_not_poll(self):
        registry = PollTests.Registry(("tau",))
        with (
            patch.object(
                sys,
                "argv",
                ["verify", "--tag", "v1.2.3", "--commit", SHA, "--check-once"],
            ),
            patch.object(ready, "GitHub", return_value=FakeGitHub()),
            patch.object(ready, "Registry", return_value=registry),
            patch.object(ready, "metadata", return_value=release()),
            patch.object(ready, "git", side_effect=[TAG_OBJECT, SHA]),
        ):
            with self.assertRaisesRegex(ready.IncompleteRelease, "image/tau"):
                ready.main()
        self.assertEqual(len(registry.calls), 9)

    def test_entrypoint_writes_summary_only_after_success(self):
        for succeeds in (True, False):
            with self.subTest(succeeds=succeeds), tempfile.TemporaryDirectory() as work:
                summary = Path(work) / "summary"
                registry = PollTests.Registry()
                if not succeeds:

                    def fail_image(*args):
                        raise artifacts.ReleaseError("image provenance mismatch")

                    registry.image = fail_image
                with (
                    patch.object(
                        sys, "argv", ["verify", "--tag", "v1.2.3", "--commit", SHA]
                    ),
                    patch.dict(os.environ, {"GITHUB_STEP_SUMMARY": str(summary)}),
                    patch.object(ready, "GitHub", return_value=FakeGitHub()),
                    patch.object(ready, "Registry", return_value=registry),
                    patch.object(ready, "metadata", return_value=release()),
                    patch.object(ready, "git", side_effect=[TAG_OBJECT, SHA]),
                ):
                    if succeeds:
                        ready.main()
                        self.assertIn("## Release ready", summary.read_text())
                        self.assertIn(SHA, summary.read_text())
                        self.assertEqual(len(registry.calls), 9)
                    else:
                        with self.assertRaisesRegex(
                            artifacts.ReleaseError, "provenance"
                        ):
                            ready.main()
                        self.assertFalse(summary.exists())

    def test_entrypoint_rechecks_remote_tag_after_polling(self):
        github = FakeGitHub()
        with (
            patch.object(sys, "argv", ["verify", "--tag", "v1.2.3"]),
            patch.object(ready, "GitHub", return_value=github),
            patch.object(
                github, "tag", side_effect=[(TAG_OBJECT, SHA), ("d" * 40, SHA)]
            ),
            patch.object(ready, "Registry", return_value=PollTests.Registry()),
            patch.object(ready, "metadata", return_value=release()),
            patch.object(ready, "git", side_effect=[TAG_OBJECT, SHA]),
        ):
            with self.assertRaisesRegex(artifacts.ReleaseError, "tag moved"):
                ready.main()

    def test_legacy_pipeline_is_manual_and_cannot_publish(self):
        text = (ROOT / ".pipelines/publish-helm-charts.yml").read_text()
        self.assertIn("trigger: none", text)
        self.assertIn("pr: none", text)
        for forbidden in (
            "AzureCLI",
            "azureSubscription",
            "acr login",
            "helm push",
            "publish-helm-charts.sh",
        ):
            self.assertNotIn(forbidden, text)
        for option in ("--tag", "--commit", "--version", "--timeout 1800"):
            self.assertIn(option, text)

    def test_public_readiness_is_separate_read_only_and_bounded(self):
        text = (ROOT / ".github/workflows/release-ready.yaml").read_text()
        self.assertIn("workflows: [Release TauGrid]", text)
        self.assertIn("workflow_dispatch:", text)
        self.assertIn("contents: read", text)
        self.assertIn("actions: read", text)
        self.assertIn("name: Release ready", text)
        self.assertIn("conclusion == 'success'", text)
        self.assertIn("timeout-minutes: 245", text)
        self.assertIn("--timeout 14400", text)
        self.assertNotIn(
            "release-ready", (ROOT / ".github/workflows/release-tau.yaml").read_text()
        )

    def test_all_four_image_pushes_receive_optional_provenance_args(self):
        for name in artifacts.IMAGES:
            output = subprocess.check_output(
                [
                    "make",
                    "-n",
                    "-C",
                    str(ROOT / "images" / name),
                    "docker-push",
                    f"DOCKER_BUILD_ARGS=--annotation index:{artifacts.REVISION}={SHA} --label {artifacts.REVISION}={SHA}",
                ],
                text=True,
            )
            with self.subTest(image=name):
                self.assertIn(f"--annotation index:{artifacts.REVISION}={SHA}", output)
                self.assertIn(f"--label {artifacts.REVISION}={SHA}", output)


if __name__ == "__main__":
    unittest.main()
