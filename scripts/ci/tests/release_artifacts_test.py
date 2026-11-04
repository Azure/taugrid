#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import copy
import hashlib
import io
import json
import shutil
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import release_artifacts as artifacts

SOURCE = "a" * 40
CONTROL = "b" * 40


def fixture_source(root):
    for name in artifacts.CHARTS:
        chart = root / "charts" / name
        chart.mkdir(parents=True)
        metadata = {"apiVersion": "v2", "name": name, "version": "1.2.3",
                    "appVersion": "1.2.3", "type": "application"}
        if name == "taugrid":
            metadata["dependencies"] = [
                {"name": dependency, "version": "1.2.3" if dependency in artifacts.CHARTS else "1.0.0",
                 "repository": f"oci://{artifacts.CHART_PREFIX}"}
                for dependency in ("kueue", "kuberay-operator", *artifacts.CHARTS[:2], "gpu-monitoring")]
        (chart / "Chart.yaml").write_text(yaml.safe_dump(metadata))
        (chart / "values.yaml").write_text("{}\n")
        (chart / "templates").mkdir()
        (chart / "templates" / "config.yaml").write_text(
            f"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {name}\n")


def fixture_upstream(name, chart_version):
    files = {"Chart.yaml": yaml.safe_dump(
        {"apiVersion": "v2", "name": name, "version": chart_version}).encode(),
        "values.yaml": b"{}\n"}
    if name == "kueue":
        files["templates/crd/sample.yaml"] = (
            b"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: upstream-crd\n"
            b"  annotations:\n    upstream: pinned\n")
    return b"upstream-manifest-" + name.encode(), artifacts.archive_files(name, files)


def fixture_image(revision=SOURCE):
    objects = {}
    manifests = []
    for platform in artifacts.PLATFORMS:
        os_name, architecture = platform.split("/")
        config = json.dumps({"os": os_name, "architecture": architecture,
                             "config": {"Labels": {"org.opencontainers.image.revision": revision}}}).encode()
        objects[("blobs", artifacts.digest(config))] = config
        manifest = json.dumps({"schemaVersion": 2, "config": {
            "digest": artifacts.digest(config), "size": len(config)}}).encode()
        objects[("manifests", artifacts.digest(manifest))] = manifest
        manifests.append({"digest": artifacts.digest(manifest), "size": len(manifest),
                          "platform": {"os": os_name, "architecture": architecture}})
    index = json.dumps({"schemaVersion": 2, "manifests": manifests}).encode()
    objects[("manifests", "1.2.3")] = index
    entry = {"name": "tau", "reference": f"{artifacts.IMAGE_PREFIX}/tau:1.2.3",
             "digest": artifacts.digest(index), "platforms": list(artifacts.PLATFORMS),
             "revision": SOURCE}
    return entry, objects


class ReleaseArtifactTests(unittest.TestCase):
    def test_exact_supported_versions(self):
        for tag in ("v1.2.3", "v0.0.0", "v1.2.3-rc.1"):
            self.assertEqual(artifacts.version(tag), tag[1:])
        for tag in ("1.2.3", "v01.2.3", "v1.2.3+build", "v1.2.3-01", "v1.2.3/x"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                artifacts.version(tag)

    def test_archive_is_deterministic(self):
        files = {"Chart.yaml": b"name: chart\n", "templates/x": b"data"}
        self.assertEqual(artifacts.archive_files("chart", files),
                         artifacts.archive_files("chart", dict(reversed(list(files.items())))))
        self.assertEqual(artifacts.chart_files(artifacts.archive_files("chart", files), "chart"), files)

    def test_archive_rejects_traversal_links_duplicates_and_wrong_root(self):
        for names, kind in ((["chart/../bad"], tarfile.REGTYPE),
                            (["chart/link"], tarfile.SYMTYPE),
                            (["other/Chart.yaml"], tarfile.REGTYPE),
                            (["chart/Chart.yaml", "chart/Chart.yaml"], tarfile.REGTYPE)):
            stream = io.BytesIO()
            with tarfile.open(fileobj=stream, mode="w:gz") as archive:
                for name in names:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    archive.addfile(member, io.BytesIO())
            with self.subTest(names=names), self.assertRaises(ValueError):
                artifacts.chart_files(stream.getvalue(), "chart")

    def test_retention_is_only_allowed_crd_transformation(self):
        _, archive = fixture_upstream("kueue", "1.0.0")
        original = artifacts.chart_files(archive, "kueue")
        original["templates/crd/sample.yaml"] += (
            b"spec:\n  nested:\n    annotations:\n      description: schema\n")
        retained = artifacts.retained_kueue(original)
        self.assertEqual(retained["Chart.yaml"], original["Chart.yaml"])
        self.assertIn(b"helm.sh/resource-policy: keep", retained["templates/crd/sample.yaml"])
        self.assertIn(b"    annotations:\n      description: schema\n",
                      retained["templates/crd/sample.yaml"])
        self.assertEqual(retained["templates/crd/sample.yaml"].count(b"resource-policy"), 1)
        self.assertEqual(artifacts.retained_kueue(retained), retained)
        with self.assertRaises(ValueError):
            artifacts.retained_kueue({"templates/crd/bad.yaml": b"no insertion point"})

    def test_public_image_requires_both_platforms_digest_and_source_config(self):
        entry, objects = fixture_image()
        with patch.object(artifacts, "public_bytes", side_effect=lambda repo, kind, ref: objects[(kind, ref)]):
            artifacts.verify_image(entry, SOURCE, "1.2.3")
            for field, value in (("digest", "sha256:" + "0" * 64),
                                 ("revision", "c" * 40), ("platforms", ["linux/amd64"]),
                                 ("reference", "other/tau:1.2.3")):
                invalid = dict(entry, **{field: value})
                with self.subTest(field=field), self.assertRaises(ValueError):
                    artifacts.verify_image(invalid, SOURCE, "1.2.3")
            with self.assertRaises(ValueError):
                artifacts.verify_image(entry, "c" * 40, "1.2.3")

    def test_image_rejects_missing_duplicate_or_unknown_platform(self):
        entry, original = fixture_image()
        for mutation in ("missing", "duplicate", "unknown"):
            objects = copy.deepcopy(original)
            index = json.loads(objects[("manifests", "1.2.3")])
            if mutation == "missing":
                index["manifests"].pop()
            elif mutation == "duplicate":
                index["manifests"].append(index["manifests"][0])
            else:
                index["manifests"][0]["platform"]["architecture"] = "unknown"
            data = json.dumps(index).encode()
            objects[("manifests", "1.2.3")] = data
            invalid = dict(entry, digest=artifacts.digest(data))
            with patch.object(artifacts, "public_bytes", side_effect=lambda repo, kind, ref: objects[(kind, ref)]):
                with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                    artifacts.verify_image(invalid, SOURCE, "1.2.3")

    def test_public_config_must_match_source_even_when_receipt_matches(self):
        entry, objects = fixture_image("d" * 40)
        with patch.object(artifacts, "public_bytes", side_effect=lambda repo, kind, ref: objects[(kind, ref)]):
            with self.assertRaisesRegex(ValueError, "public image revision mismatch"):
                artifacts.verify_image(entry, SOURCE, "1.2.3")

    def test_receipt_identity_and_matrix_are_closed(self):
        receipt = {"schema": 1, "sourceCommit": SOURCE, "controlCommit": CONTROL,
                   "releaseTag": "1.2.3", "pipelineId": 1, "runId": 2,
                   "images": [{"name": name} for name in artifacts.IMAGES],
                   "charts": [{"name": name} for name in artifacts.CHARTS]}
        for field, value in (("sourceCommit", CONTROL), ("controlCommit", SOURCE),
                             ("pipelineId", 2), ("runId", 3), ("releaseTag", "2.0.0"),
                             ("schema", 2), ("images", []), ("charts", [{"name": "taugrid"}])):
            with self.subTest(field=field), self.assertRaises(ValueError):
                artifacts.verify_receipt(dict(receipt, **{field: value}), Path("."),
                                         "v1.2.3", SOURCE, CONTROL, 1, 2)

    @unittest.skipUnless(shutil.which("helm"), "Helm is required for chart integration")
    def test_chart_packaging_and_receipt_verification_end_to_end(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source"
            fixture_source(source)
            first, second = root / "first", root / "second"
            with patch.object(artifacts, "public_chart", side_effect=fixture_upstream):
                artifacts.package_charts(source, "v1.2.3", first)
                artifacts.package_charts(source, "v1.2.3", second)
                with self.assertRaises(ValueError):
                    artifacts.package_charts(source, "v1.2.3", first)
            self.assertEqual({path.name for path in first.iterdir()},
                             {f"{name}-1.2.3.tgz" for name in artifacts.CHARTS})
            charts = []
            for name in artifacts.CHARTS:
                archive = (first / f"{name}-1.2.3.tgz").read_bytes()
                self.assertEqual(archive, (second / f"{name}-1.2.3.tgz").read_bytes())
                charts.append({"name": name, "reference": f"{artifacts.CHART_PREFIX}/{name}:1.2.3",
                               "digest": artifacts.digest(name.encode()),
                               "contentSha256": hashlib.sha256(archive).hexdigest()})
            receipt = {"schema": 1, "sourceCommit": SOURCE, "controlCommit": CONTROL,
                       "releaseTag": "1.2.3", "pipelineId": 1, "runId": 2,
                       "images": [{"name": name, "reference": f"{artifacts.IMAGE_PREFIX}/{name}:1.2.3",
                                   "digest": "sha256:" + "0" * 64,
                                   "platforms": list(artifacts.PLATFORMS), "revision": SOURCE}
                                  for name in artifacts.IMAGES], "charts": charts}
            receipt["images"][0]["privatePublisherMetadata"] = "not-for-public-evidence"

            def public_chart(name, chart_version):
                if name in artifacts.CHARTS:
                    return name.encode(), (first / f"{name}-1.2.3.tgz").read_bytes()
                return fixture_upstream(name, chart_version)

            with patch.object(artifacts, "public_chart", side_effect=public_chart), \
                    patch.object(artifacts, "verify_image"):
                evidence = artifacts.verify_receipt(receipt, source, "v1.2.3", SOURCE, CONTROL, 1, 2)
                self.assertNotIn("pipelineId", evidence)
                self.assertNotIn("controlCommit", evidence)
                self.assertNotIn("not-for-public-evidence", json.dumps(evidence))
                self.assertEqual({entry["name"] for entry in evidence["dependencies"]},
                                 {"kueue", "kuberay-operator", "gpu-monitoring"})
                archive = (first / "taugrid-1.2.3.tgz").read_bytes()
                files = artifacts.chart_files(archive, "taugrid")
                files["templates/config.yaml"] += b"\ndata:\n  changed: true\n"
                tampered = artifacts.archive_files("taugrid", files)
                receipt["charts"][2]["contentSha256"] = hashlib.sha256(tampered).hexdigest()

                def tampered_public_chart(name, chart_version):
                    if name == "taugrid":
                        return name.encode(), tampered
                    return public_chart(name, chart_version)

                with patch.object(artifacts, "public_chart", side_effect=tampered_public_chart):
                    with self.assertRaises(ValueError):
                        artifacts.verify_receipt(receipt, source, "v1.2.3", SOURCE, CONTROL, 1, 2)

    def test_independent_runtime_pins_are_verified_not_rebuilt(self):
        data = {"images": [f"{artifacts.IMAGE_PREFIX}/ray:py3.12-ray2.58.0-cuda13.0"],
                "metrics": {"repository": f"{artifacts.IMAGE_PREFIX}/gpu-metrics-collector",
                            "digest": "sha256:" + "1" * 64},
                "operator": {"repository": "mcr.microsoft.com/oss/operator", "tag": "v1.0.0"}}
        references = artifacts.runtime_references(data)
        self.assertEqual(len(references), 3)
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)
            fixture_source(source)
            (source / "charts/taugrid-core/values.yaml").write_text(yaml.safe_dump(data))
            with patch.object(artifacts, "public_bytes",
                              return_value=b'{"schemaVersion":2,"config":{}}') as read:
                evidence = artifacts.verify_runtime_pins(source, "v1.2.3")
                self.assertEqual({entry["reference"] for entry in evidence}, references)
                self.assertEqual(read.call_count, 3)
            with patch.object(artifacts, "public_bytes", side_effect=ValueError("not found")):
                with self.assertRaises(ValueError):
                    artifacts.verify_runtime_pins(source, "v1.2.3")


if __name__ == "__main__":
    unittest.main()
