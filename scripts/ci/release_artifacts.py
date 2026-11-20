#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile

CHARTS = ("tau-core-controller", "gpu-monitoring", "taugrid-core", "adx-mon", "taugrid")
PRODUCT_CHARTS = ("tau-core-controller", "taugrid-core", "taugrid")
IMAGES = ("tau", "taugrid-portal", "tau-core-controller", "taugrid-metrics-collector")
REVISION = "org.opencontainers.image.revision"
SOURCE_TREE = "org.taugrid.chart.source-tree"
STABLE = r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"


class ReleaseError(RuntimeError):
    pass


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def scalar(value):
    value = value.strip()
    if not value:
        raise ReleaseError("required Chart.yaml scalar is empty")
    if value.startswith('"'):
        decoded = json.loads(value)
        if not isinstance(decoded, str):
            raise ReleaseError("required Chart.yaml scalar must be a string")
        return decoded
    if value.startswith("'"):
        if len(value) < 2 or not value.endswith("'"):
            raise ReleaseError("invalid single-quoted Chart.yaml scalar")
        return value[1:-1].replace("''", "'")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._+-]*", value):
        raise ReleaseError("unsupported Chart.yaml scalar")
    return value


def chart_metadata(data, name):
    text = data.decode("utf-8")
    fields = {}
    annotations = {}
    lines = text.splitlines()
    index = 0
    while index < len(lines):
        line = lines[index]
        match = re.fullmatch(r"([A-Za-z][A-Za-z0-9_-]*):(?:[ \t]*(.*))?", line)
        if not match:
            index += 1
            continue
        key, value = match.groups()
        if key in ("name", "version", "appVersion"):
            if key in fields:
                raise ReleaseError(f"duplicate {key} in {name}/Chart.yaml")
            fields[key] = scalar(value or "")
        elif key == "annotations":
            if key in fields or (value or "").strip():
                raise ReleaseError(f"invalid annotations in {name}/Chart.yaml")
            fields[key] = True
            index += 1
            while index < len(lines):
                child = lines[index]
                if child and not child[0].isspace():
                    index -= 1
                    break
                annotation = re.fullmatch(
                    r"  ([A-Za-z0-9][A-Za-z0-9._/-]*):[ \t]*(.*)", child
                )
                if annotation:
                    annotation_key, annotation_value = annotation.groups()
                    if annotation_key in annotations:
                        raise ReleaseError(
                            f"duplicate annotation in {name}/Chart.yaml"
                        )
                    annotations[annotation_key] = scalar(annotation_value)
                elif child.strip() and not child.lstrip().startswith("#"):
                    raise ReleaseError(
                        f"unsupported annotations entry in {name}/Chart.yaml"
                    )
                index += 1
        index += 1
    if "name" not in fields or "version" not in fields:
        raise ReleaseError(f"missing chart identity: {name}")
    return {
        "name": fields["name"],
        "version": fields["version"],
        "appVersion": fields.get("appVersion"),
        "annotations": annotations,
    }


def stamp_chart(data, name, release):
    text = data.decode("utf-8")
    chart_metadata(data, name)
    lines = text.splitlines()
    annotations = {
        REVISION: release["commit"],
        SOURCE_TREE: release["charts"][name]["source_tree"],
    }
    annotation_index = next(
        (index for index, line in enumerate(lines) if line == "annotations:"),
        None,
    )
    if annotation_index is None:
        if lines and lines[-1]:
            lines.append("")
        lines.append("annotations:")
        lines.extend(
            f"  {key}: {json.dumps(value)}" for key, value in annotations.items()
        )
    else:
        block_end = annotation_index + 1
        while block_end < len(lines) and (
            not lines[block_end] or lines[block_end][0].isspace()
        ):
            block_end += 1
        for key, value in annotations.items():
            prefix = f"  {key}:"
            matches = [
                index
                for index in range(annotation_index + 1, block_end)
                if lines[index].startswith(prefix)
            ]
            if len(matches) > 1:
                raise ReleaseError(f"duplicate annotation in {name}/Chart.yaml")
            replacement = f"{prefix} {json.dumps(value)}"
            if matches:
                lines[matches[0]] = replacement
            else:
                lines.insert(block_end, replacement)
                block_end += 1
    return ("\n".join(lines) + "\n").encode()


def metadata(commit, version):
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ReleaseError("release source commit must be a full lowercase SHA")
    if not re.fullmatch(STABLE, version):
        raise ReleaseError("release version must be stable X.Y.Z")
    charts = {}
    for name in CHARTS:
        path = f"charts/{name}"
        chart = chart_metadata(
            git("show", f"{commit}:{path}/Chart.yaml").encode(),
            name,
        )
        if chart["name"] != name or not re.fullmatch(STABLE, str(chart["version"])):
            raise ReleaseError(f"invalid chart identity: {name}")
        if name in PRODUCT_CHARTS and (
            str(chart["version"]) != version or str(chart["appVersion"]) != version
        ):
            raise ReleaseError(f"{name} version and appVersion must equal {version}")
        charts[name] = {
            "version": str(chart["version"]),
            "source_tree": git("rev-parse", f"{commit}:{path}"),
        }
    return {"commit": commit, "version": version, "charts": charts}


def archive_chart(data, name):
    with gzip.GzipFile(fileobj=io.BytesIO(data)) as compressed:
        unpacked = compressed.read(64 * 1024 * 1024 + 1)
    if len(unpacked) > 64 * 1024 * 1024:
        raise ReleaseError(f"unpacked chart exceeds size limit: {name}")
    with tarfile.open(fileobj=io.BytesIO(unpacked), mode="r:") as archive:
        entries = [
            entry
            for entry in archive.getmembers()
            if entry.name == f"{name}/Chart.yaml"
        ]
        if (
            len(entries) != 1
            or not entries[0].isfile()
            or entries[0].size > 1024 * 1024
        ):
            raise ReleaseError(f"invalid {name}/Chart.yaml in package")
        return chart_metadata(archive.extractfile(entries[0]).read(), name)


def verify_chart(data, name, release):
    chart = archive_chart(data, name)
    expected = release["charts"][name]
    annotations = chart.get("annotations", {})
    if chart.get("name") != name or str(chart.get("version")) != expected["version"]:
        raise ReleaseError(f"chart identity mismatch: {name}")
    if annotations.get(SOURCE_TREE) != expected["source_tree"]:
        raise ReleaseError(f"chart source-tree mismatch or missing annotation: {name}")
    if not re.fullmatch(r"[0-9a-f]{40}", str(annotations.get(REVISION, ""))):
        raise ReleaseError(f"chart source revision must be a full SHA: {name}")
    if name in PRODUCT_CHARTS and (
        annotations.get(REVISION) != release["commit"]
        or str(chart.get("appVersion")) != release["version"]
    ):
        raise ReleaseError(f"chart release provenance mismatch: {name}")


def seal(directory, release):
    hashes = {}
    for name, chart in release["charts"].items():
        path = directory / f"{name}-{chart['version']}.tgz"
        data = path.read_bytes()
        output = io.BytesIO()
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as source:
            with tarfile.open(fileobj=output, mode="w:gz") as target:
                for entry in source:
                    content = source.extractfile(entry) if entry.isfile() else None
                    if entry.name == f"{name}/Chart.yaml":
                        encoded = stamp_chart(content.read(), name, release)
                        entry.size = len(encoded)
                        content = io.BytesIO(encoded)
                    target.addfile(entry, content)
        stamped = output.getvalue()
        verify_chart(stamped, name, release)
        path.write_bytes(stamped)
        hashes[path.name] = hashlib.sha256(stamped).hexdigest()
    (directory / "release-manifest.json").write_text(
        json.dumps({**release, "packages": hashes}, indent=2) + "\n"
    )


def verify_directory(directory, release):
    manifest = json.loads((directory / "release-manifest.json").read_text())
    if {key: manifest.get(key) for key in release} != release:
        raise ReleaseError("package manifest does not match release source/version")
    expected_files = {
        f"{name}-{chart['version']}.tgz" for name, chart in release["charts"].items()
    }
    if set(manifest["packages"]) != expected_files:
        raise ReleaseError(
            "package manifest must include exactly all five release charts"
        )
    for name, chart in release["charts"].items():
        filename = f"{name}-{chart['version']}.tgz"
        data = (directory / filename).read_bytes()
        if hashlib.sha256(data).hexdigest() != manifest["packages"][filename]:
            raise ReleaseError(f"package bytes changed since sealing: {filename}")
        verify_chart(data, name, release)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "command", choices=("validate-source", "seal", "verify", "verify-archive")
    )
    parser.add_argument("path", nargs="?", type=Path)
    parser.add_argument("chart", nargs="?", choices=CHARTS)
    args = parser.parse_args()
    release = metadata(
        os.environ.get("RELEASE_SOURCE_COMMIT", ""),
        os.environ.get("RELEASE_VERSION", ""),
    )
    if git("rev-parse", "HEAD") != release["commit"]:
        raise ReleaseError("checkout HEAD must equal RELEASE_SOURCE_COMMIT")
    if git("diff", "--name-only", release["commit"], "--", "charts"):
        raise ReleaseError("tracked chart inputs differ from release source")
    if args.command == "validate-source" and git(
        "status", "--porcelain", "--untracked-files=all", "--", "charts"
    ):
        raise ReleaseError("release charts must come from a clean checkout")
    if args.command == "seal":
        seal(args.path, release)
    elif args.command == "verify":
        verify_directory(args.path, release)
    elif args.command == "verify-archive":
        verify_chart(args.path.read_bytes(), args.chart, release)


if __name__ == "__main__":
    try:
        main()
    except (
        ReleaseError,
        AttributeError,
        KeyError,
        TypeError,
        ValueError,
        UnicodeError,
        OSError,
        tarfile.TarError,
        subprocess.CalledProcessError,
    ) as error:
        raise SystemExit(f"release artifacts: {error}") from error
