#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import gzip
import hashlib
import io
import json
import re
import shutil
import subprocess
import tarfile
import tempfile
import urllib.error
import urllib.request
from pathlib import Path, PurePosixPath

import yaml

IMAGES = ("tau", "taugrid-portal", "tau-core-controller", "taugrid-metrics-collector")
CHARTS = ("tau-core-controller", "taugrid-core", "taugrid")
PLATFORMS = ("linux/amd64", "linux/arm64")
REGISTRY = "mcr.microsoft.com"
IMAGE_PREFIX = f"{REGISTRY}/aks/ai-runtime"
CHART_PREFIX = f"{IMAGE_PREFIX}/helm"
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
SHA = re.compile(r"[0-9a-f]{40}")
TAG = re.compile(
    r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    r"(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?"
)


class ArtifactNotFound(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise ValueError(message)


def version(tag):
    require(TAG.fullmatch(tag) is not None and len(tag) <= 128,
            "release tag must be v-prefixed SemVer without build metadata")
    return tag[1:]


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def download(url, accept="application/json"):
    request = urllib.request.Request(url, headers={"Accept": accept})
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            require(response.status == 200, "public registry returned unexpected status")
            return response.read(64 * 1024 * 1024 + 1)
    except urllib.error.HTTPError as error:
        if error.code == 404:
            raise ArtifactNotFound("public artifact does not exist") from None
        raise ValueError("public artifact read failed; readiness is unknown") from error
    except urllib.error.URLError as error:
        raise ValueError("public artifact read failed; readiness is unknown") from error


def public_bytes(repository, kind, reference):
    require(repository.startswith(REGISTRY + "/") and
            re.fullmatch(r"[a-z0-9/._-]+", repository), "unsupported public repository")
    require(kind in ("manifests", "blobs"), "unsupported registry request")
    require(re.fullmatch(r"[A-Za-z0-9_.:-]+", reference), "invalid registry reference")
    accept = ", ".join((
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ))
    data = download(f"https://{REGISTRY}/v2/{repository.split('/', 1)[1]}/{kind}/{reference}", accept)
    require(len(data) <= 64 * 1024 * 1024, "public artifact exceeds size limit")
    if reference.startswith("sha256:"):
        require(digest(data) == reference, "public content digest mismatch")
    return data


def descriptor_bytes(repository, descriptor, kind):
    require(isinstance(descriptor, dict) and
            DIGEST.fullmatch(descriptor.get("digest", "")) is not None,
            "missing immutable descriptor digest")
    data = public_bytes(repository, kind, descriptor["digest"])
    require(type(descriptor.get("size")) is int and len(data) == descriptor["size"],
            "public descriptor size mismatch")
    return data


def public_chart(name, chart_version):
    repository = f"{CHART_PREFIX}/{name}"
    manifest_data = public_bytes(repository, "manifests", chart_version)
    manifest = json.loads(manifest_data)
    require(manifest.get("schemaVersion") == 2, "invalid Helm OCI manifest")
    require(manifest.get("config", {}).get("mediaType") ==
            "application/vnd.cncf.helm.config.v1+json", "invalid Helm OCI config")
    metadata = json.loads(descriptor_bytes(repository, manifest["config"], "blobs"))
    require(metadata.get("name") == name and metadata.get("version") == chart_version,
            "public chart metadata mismatch")
    layers = manifest.get("layers", [])
    require(len(layers) == 1 and layers[0].get("mediaType") ==
            "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
            "expected one Helm chart layer")
    archive = descriptor_bytes(repository, layers[0], "blobs")
    files = chart_files(archive, name)
    require(yaml.safe_load(files["Chart.yaml"]).get("version") == chart_version,
            "packaged chart version mismatch")
    return manifest_data, archive


def chart_files(archive, name):
    files = {}
    total = 0
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as chart:
        for member in chart:
            path = PurePosixPath(member.name)
            require(bool(path.parts) and not path.is_absolute() and ".." not in path.parts and
                    path.parts[0] == name, "unsafe chart archive path")
            if member.isdir():
                continue
            require(member.isfile() and len(path.parts) > 1, "unsupported chart archive member")
            relative = str(PurePosixPath(*path.parts[1:]))
            require(relative not in files, "duplicate chart archive member")
            total += member.size
            require(total <= 128 * 1024 * 1024, "expanded chart exceeds size limit")
            files[relative] = chart.extractfile(member).read()
    require("Chart.yaml" in files, "chart archive has no metadata")
    return files


def retained_kueue(files):
    result = dict(files)
    crds = [path for path in files if path.startswith("templates/crd/") and path.endswith(".yaml")]
    require(crds, "pinned Kueue chart has no CRDs")
    for path in crds:
        content = result[path]
        if b"helm.sh/resource-policy: keep" not in content:
            insertion = rb"(?m)^  annotations:\n"
            require(len(re.findall(insertion, content)) == 1,
                    "Kueue CRD has no unique metadata annotation insertion point")
            result[path] = re.sub(
                insertion, b"  annotations:\n    helm.sh/resource-policy: keep\n", content)
    return result


def archive_files(name, files):
    stream = io.BytesIO()
    with gzip.GzipFile(fileobj=stream, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as archive:
            for path, content in sorted(files.items()):
                member = tarfile.TarInfo(f"{name}/{path}")
                member.size = len(content)
                member.mode = 0o644
                archive.addfile(member, io.BytesIO(content))
    return stream.getvalue()


def source_charts(source, tag):
    release_version = version(tag)
    metadata = {}
    for name in CHARTS:
        chart = yaml.safe_load((source / "charts" / name / "Chart.yaml").read_text())
        require(chart.get("name") == name and chart.get("version") == release_version and
                chart.get("appVersion") == release_version, f"{name} release version mismatch")
        metadata[name] = chart
    dependencies = metadata["taugrid"].get("dependencies", [])
    names = [entry["name"] for entry in dependencies]
    require(len(names) == len(set(names)) and set(names) ==
            {"kueue", "kuberay-operator", "gpu-monitoring", *CHARTS[:2]},
            "unsupported coordinated dependency matrix")
    for entry in dependencies:
        require(entry.get("repository") == f"oci://{CHART_PREFIX}",
                "dependency must use an exact public chart repository")
        version("v" + str(entry.get("version", "")))
        if entry["name"] in CHARTS:
            require(entry["version"] == release_version, "coordinated dependency version mismatch")
    require(not metadata[CHARTS[0]].get("dependencies") and
            not metadata[CHARTS[1]].get("dependencies"), "unexpected standalone chart dependencies")
    return metadata


def package_charts(source, tag, output, dependencies=None, runtime_values=None):
    metadata = source_charts(source, tag)
    release_version = version(tag)
    output.mkdir(parents=True, exist_ok=True)
    require(not any((output / f"{name}-{release_version}.tgz").exists() for name in CHARTS),
            "refusing to overwrite chart packages")
    with tempfile.TemporaryDirectory() as temporary:
        work = Path(temporary)
        packaged = work / "packages"
        packaged.mkdir()
        for name in CHARTS:
            chart = work / name
            shutil.copytree(source / "charts" / name, chart,
                            ignore=shutil.ignore_patterns("charts", ".git"))
            if name == "taugrid":
                vendor = chart / "charts"
                vendor.mkdir()
                for entry in metadata[name]["dependencies"]:
                    dependency = entry["name"]
                    filename = f"{dependency}-{entry['version']}.tgz"
                    if dependency in CHARTS:
                        archive = (packaged / filename).read_bytes()
                    else:
                        manifest, archive = public_chart(dependency, entry["version"])
                        if runtime_values is not None:
                            dependency_files = chart_files(archive, dependency)
                            require("values.yaml" in dependency_files,
                                    "pinned dependency chart has no default values")
                            runtime_values.append(yaml.safe_load(dependency_files["values.yaml"]))
                        if dependencies is not None:
                            dependencies.append(
                                {"name": dependency, "version": entry["version"],
                                 "digest": digest(manifest),
                                 "contentSha256": hashlib.sha256(archive).hexdigest()})
                        if dependency == "kueue":
                            archive = archive_files(dependency, retained_kueue(chart_files(archive, dependency)))
                    (vendor / filename).write_bytes(archive)
            subprocess.run(["helm", "lint", str(chart)], check=True)
            subprocess.run(["helm", "template", "release-check", str(chart),
                            "--namespace", "taugrid-system"], check=True, stdout=subprocess.DEVNULL)
            subprocess.run(["helm", "package", str(chart), "--destination", str(packaged)], check=True)
            filename = packaged / f"{name}-{release_version}.tgz"
            filename.write_bytes(archive_files(name, chart_files(filename.read_bytes(), name)))
        for name in CHARTS:
            filename = f"{name}-{release_version}.tgz"
            with (output / filename).open("xb") as destination:
                destination.write((packaged / filename).read_bytes())


def verify_image(entry, source_commit, release_version):
    require(entry.get("reference") == f"{IMAGE_PREFIX}/{entry['name']}:{release_version}",
            "image reference mismatch")
    require(sorted(entry.get("platforms", [])) == sorted(PLATFORMS) and
            entry.get("revision") == source_commit, "image receipt provenance mismatch")
    repository = f"{IMAGE_PREFIX}/{entry['name']}"
    index_data = public_bytes(repository, "manifests", release_version)
    require(digest(index_data) == entry.get("digest"), "public image index differs from receipt")
    index = json.loads(index_data)
    require(index.get("schemaVersion") == 2 and isinstance(index.get("manifests"), list),
            "public image must have a platform index")
    platforms = {}
    for descriptor in index["manifests"]:
        platform = descriptor.get("platform", {})
        key = f"{platform.get('os')}/{platform.get('architecture')}"
        if key == "unknown/unknown":
            require(descriptor.get("annotations", {}).get("vnd.docker.reference.type") ==
                    "attestation-manifest", "unexpected unknown platform")
            continue
        require(key in PLATFORMS and key not in platforms, "unexpected or duplicate image platform")
        manifest = json.loads(descriptor_bytes(repository, descriptor, "manifests"))
        config = json.loads(descriptor_bytes(repository, manifest["config"], "blobs"))
        require(f"{config.get('os')}/{config.get('architecture')}" == key,
                "image platform config mismatch")
        require(config.get("config", {}).get("Labels", {}).get("org.opencontainers.image.revision") ==
                source_commit, "public image revision mismatch")
        platforms[key] = descriptor["digest"]
    require(set(platforms) == set(PLATFORMS), "public image platform missing")


def collect_image(name, source_commit, release_version):
    require(name in IMAGES, "unsupported coordinated image")
    repository = f"{IMAGE_PREFIX}/{name}"
    index_data = public_bytes(repository, "manifests", release_version)
    entry = {
        "name": name,
        "reference": f"{repository}:{release_version}",
        "digest": digest(index_data),
        "platforms": list(PLATFORMS),
        "revision": source_commit,
    }
    verify_image(entry, source_commit, release_version)
    return entry


def collect_chart(name, release_version):
    require(name in CHARTS, "unsupported coordinated chart")
    manifest, archive = public_chart(name, release_version)
    return {
        "name": name,
        "reference": f"{CHART_PREFIX}/{name}:{release_version}",
        "digest": digest(manifest),
        "contentSha256": hashlib.sha256(archive).hexdigest(),
    }


def chart_equivalent(actual, expected):
    require(set(actual) == set(expected), "chart file set mismatch")
    for path in actual:
        if PurePosixPath(path).name in ("Chart.yaml", "Chart.lock"):
            require(yaml.safe_load(actual[path]) == yaml.safe_load(expected[path]),
                    f"chart metadata mismatch: {path}")
        else:
            require(actual[path] == expected[path], f"chart content mismatch: {path}")


def runtime_references(value):
    references = set()
    if isinstance(value, dict):
        repository = value.get("repository", "")
        if isinstance(repository, str) and repository.startswith(REGISTRY + "/"):
            image_digest = value.get("digest")
            tag = value.get("tag")
            if image_digest:
                require(DIGEST.fullmatch(image_digest) is not None, "invalid pinned runtime digest")
                references.add(f"{repository}@{image_digest}")
            elif tag:
                references.add(f"{repository}:{tag}")
        for child in value.values():
            references.update(runtime_references(child))
    elif isinstance(value, list):
        for child in value:
            references.update(runtime_references(child))
    elif isinstance(value, str):
        references.update(re.findall(
            r"mcr\.microsoft\.com/[a-z0-9/._-]+(?:@sha256:[0-9a-f]{64}|:[A-Za-z0-9_.-]+)", value))
    return references


def verify_runtime_pins(source, tag, dependency_values=()):
    references = set()
    for name in (*CHARTS, "adx-mon"):
        path = source / "charts" / name / "values.yaml"
        if path.exists():
            references.update(runtime_references(yaml.safe_load(path.read_text())))
    for values in dependency_values:
        references.update(runtime_references(values))
    evidence = []
    for reference in sorted(references):
        repository, pin = reference.rsplit("@", 1) if "@" in reference else reference.rsplit(":", 1)
        if repository in {f"{IMAGE_PREFIX}/{name}" for name in IMAGES}:
            require(pin == version(tag), "coordinated runtime image version mismatch")
            continue
        require(pin not in ("latest", "main", "dev"), "runtime image must use a pinned version")
        data = public_bytes(repository, "manifests", pin)
        manifest = json.loads(data)
        require(manifest.get("schemaVersion") == 2 and
                ("manifests" in manifest or "config" in manifest), "invalid pinned runtime manifest")
        evidence.append({"reference": reference, "digest": digest(data)})
    return evidence


def verify_receipt(receipt, source, tag, source_commit, control_commit, pipeline_id, run_id):
    release_version = version(tag)
    require(type(receipt.get("schema")) is int and receipt["schema"] == 1 and
            receipt.get("sourceCommit") == source_commit and
            receipt.get("releaseTag") == release_version and
            receipt.get("controlCommit") == control_commit and
            type(receipt.get("pipelineId")) is int and receipt["pipelineId"] == pipeline_id and
            type(receipt.get("runId")) is int and receipt["runId"] == run_id,
            "publisher receipt identity mismatch")
    for key, names in (("images", IMAGES), ("charts", CHARTS)):
        entries = receipt.get(key)
        require(isinstance(entries, list) and len(entries) == len(names) and
                {entry.get("name") for entry in entries} == set(names),
                f"publisher receipt {key} matrix mismatch")
    for entry in receipt["images"]:
        verify_image(entry, source_commit, release_version)
    metadata = source_charts(source, tag)
    published = {}
    evidence = {"schema": 1, "sourceCommit": source_commit, "releaseTag": release_version,
                "images": [{key: entry[key] for key in
                            ("name", "reference", "digest", "platforms", "revision")}
                           for entry in receipt["images"]],
                "charts": [{key: entry[key] for key in
                            ("name", "reference", "digest", "contentSha256")}
                           for entry in receipt["charts"]],
                "dependencies": []}
    for entry in receipt["charts"]:
        name = entry["name"]
        require(entry.get("reference") == f"{CHART_PREFIX}/{name}:{release_version}",
                "chart reference mismatch")
        manifest, archive = public_chart(name, release_version)
        require(digest(manifest) == entry.get("digest") and
                hashlib.sha256(archive).hexdigest() == entry.get("contentSha256"),
                "public chart digest differs from receipt")
        files = chart_files(archive, name)
        require(yaml.safe_load(files["Chart.yaml"]) == metadata[name],
                "public chart source metadata mismatch")
        published[name] = files
    with tempfile.TemporaryDirectory() as temporary:
        runtime_values = []
        package_charts(source, tag, Path(temporary), evidence["dependencies"], runtime_values)
        for name in CHARTS:
            expected = chart_files((Path(temporary) / f"{name}-{release_version}.tgz").read_bytes(), name)
            chart_equivalent(published[name], expected)
    evidence["runtimePins"] = verify_runtime_pins(source, tag, runtime_values)
    return evidence


def collect_release(source, tag, source_commit):
    release_version = version(tag)
    receipt = {
        "schema": 1,
        "sourceCommit": source_commit,
        "releaseTag": release_version,
        "controlCommit": source_commit,
        "pipelineId": 1,
        "runId": 1,
        "images": [collect_image(name, source_commit, release_version) for name in IMAGES],
        "charts": [collect_chart(name, release_version) for name in CHARTS],
    }
    return verify_receipt(receipt, source, tag, source_commit, source_commit, 1, 1)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=["package-charts", "verify-release"])
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source-commit")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "package-charts":
        package_charts(args.source.resolve(), args.tag, args.output.resolve())
    else:
        require(SHA.fullmatch(args.source_commit or "") is not None,
                "release source must be a full immutable SHA")
        evidence = collect_release(args.source.resolve(), args.tag, args.source_commit)
        args.output.write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
