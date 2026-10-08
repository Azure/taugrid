#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Generate and validate SPDX SBOM assets for a coordinated TauGrid release."""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "2.0"
INDEX_FILENAME = "taugrid-release-sbom-index.json"
IMAGE_PLATFORMS = ("linux/amd64", "linux/arm64")
CLI_TARGETS = (
    ("tau", "darwin-amd64", "tau-darwin-amd64"),
    ("tau", "darwin-arm64", "tau-darwin-arm64"),
    ("tau", "linux-amd64", "tau-linux-amd64"),
    ("tau", "linux-arm64", "tau-linux-arm64"),
    ("tau", "windows-amd64", "tau-windows-amd64.exe"),
    ("tau-gen", "darwin-amd64", "tau-gen-darwin-amd64"),
    ("tau-gen", "darwin-arm64", "tau-gen-darwin-arm64"),
    ("tau-gen", "linux-amd64", "tau-gen-linux-amd64"),
    ("tau-gen", "linux-arm64", "tau-gen-linux-arm64"),
    ("tau-gen", "windows-amd64", "tau-gen-windows-amd64.exe"),
)
IMAGE_SPECS = (
    (
        "tau",
        "taugrid-core",
        "charts/taugrid-core/Chart.yaml",
        "charts/taugrid-core/values.yaml",
        ("lifecycleRecorder", "image"),
    ),
    (
        "taugrid-portal",
        "taugrid-core",
        "charts/taugrid-core/Chart.yaml",
        "charts/taugrid-core/values.yaml",
        ("portal", "image"),
    ),
    (
        "tau-core-controller",
        "tau-core-controller",
        "charts/tau-core-controller/Chart.yaml",
        "charts/tau-core-controller/values.yaml",
        ("image",),
    ),
)
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SEMVER_RE = re.compile(
    r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$"
)


class SbomError(RuntimeError):
    pass


def fail(message: str) -> None:
    raise SbomError(message)


def read_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot read valid JSON from {path}: {exc}")


def write_json(path: Path, value: Any) -> None:
    path.write_text(
        json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def valid_timestamp(value: Any) -> bool:
    if not isinstance(value, str):
        return False
    try:
        parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return False
    return parsed.tzinfo is not None


def yaml_scalar(path: Path, key: str) -> str:
    pattern = re.compile(rf"^{re.escape(key)}:\s*[\"']?([^\"'#\s]+)")
    for line in path.read_text(encoding="utf-8").splitlines():
        match = pattern.match(line)
        if match:
            return match.group(1)
    fail(f"{path} has no top-level {key}")


def yaml_mapping(path: Path, keys: tuple[str, ...]) -> dict[str, str]:
    lines = path.read_text(encoding="utf-8").splitlines()
    start = 0
    parent_indent = -1
    for key in keys:
        found = False
        pattern = re.compile(rf"^(\s*){re.escape(key)}:\s*(?:#.*)?$")
        for index in range(start, len(lines)):
            match = pattern.match(lines[index])
            indent = len(match.group(1)) if match else -1
            expected_indent = 0 if parent_indent < 0 else parent_indent + 2
            if match and indent == expected_indent:
                parent_indent = len(match.group(1))
                start = index + 1
                found = True
                break
        if not found:
            fail(f"{path} has no mapping at {'.'.join(keys)}")

    result: dict[str, str] = {}
    scalar = re.compile(r"^(\s*)(repository|tag):\s*[\"']?([^\"'#\s]+)")
    for line in lines[start:]:
        if line.strip() and len(line) - len(line.lstrip()) <= parent_indent:
            break
        match = scalar.match(line)
        if match and len(match.group(1)) > parent_indent:
            result[match.group(2)] = match.group(3)
    if set(result) != {"repository", "tag"}:
        fail(f"{path} mapping {'.'.join(keys)} must set repository and tag")
    return result


def image_specs(repo_root: Path) -> list[dict[str, str]]:
    result = []
    for component, chart, chart_rel, values_rel, keys in IMAGE_SPECS:
        chart_path = repo_root / chart_rel
        values_path = repo_root / values_rel
        chart_version = yaml_scalar(chart_path, "version")
        app_version = yaml_scalar(chart_path, "appVersion")
        image = yaml_mapping(values_path, keys)
        if not SEMVER_RE.fullmatch(chart_version):
            fail(f"{chart_path} has invalid chart version {chart_version!r}")
        if app_version != chart_version:
            fail(
                f"{chart_path} appVersion {app_version!r} does not match "
                f"chart version {chart_version!r}"
            )
        if image["tag"] != app_version:
            fail(
                f"{values_path} {component} tag {image['tag']!r} does not match "
                f"appVersion {app_version!r}"
            )
        result.append(
            {
                "component": component,
                "chart": chart,
                "chartVersion": chart_version,
                "componentVersion": app_version,
                "tagReference": f"{image['repository']}:{image['tag']}",
            }
        )
    return result


def parse_bearer_challenge(value: str) -> tuple[str, dict[str, str]]:
    if not value.startswith("Bearer "):
        fail(f"registry returned unsupported authentication challenge {value!r}")
    params = {}
    for item in re.findall(r'([a-zA-Z]+)="([^"]*)"', value[7:]):
        params[item[0]] = item[1]
    realm = params.pop("realm", "")
    if not realm:
        fail("registry bearer challenge has no realm")
    return realm, params


def registry_request(url: str, headers: dict[str, str]) -> urllib.response.addinfourl:
    request = urllib.request.Request(url, headers=headers, method="GET")
    try:
        return urllib.request.urlopen(request, timeout=60)
    except urllib.error.HTTPError as exc:
        if exc.code != 401:
            raise
        challenge = exc.headers.get("WWW-Authenticate", "")
        realm, params = parse_bearer_challenge(challenge)
        token_url = f"{realm}?{urllib.parse.urlencode(params)}"
        with urllib.request.urlopen(token_url, timeout=60) as token_response:
            token_doc = json.load(token_response)
        token = token_doc.get("token") or token_doc.get("access_token")
        if not token:
            fail("registry token response contains no token")
        authenticated = dict(headers)
        authenticated["Authorization"] = f"Bearer {token}"
        return urllib.request.urlopen(
            urllib.request.Request(url, headers=authenticated, method="GET"),
            timeout=60,
        )


def resolve_image(reference: str) -> dict[str, Any]:
    if "@" in reference:
        fail(f"expected a tag reference before resolution, got {reference!r}")
    repository, separator, tag = reference.rpartition(":")
    if not separator or "/" not in repository or not tag:
        fail(f"invalid tagged image reference {reference!r}")
    registry, _, path = repository.partition("/")
    url = f"https://{registry}/v2/{path}/manifests/{urllib.parse.quote(tag, safe='')}"
    headers = {
        "Accept": ", ".join(
            (
                "application/vnd.oci.image.index.v1+json",
                "application/vnd.oci.image.manifest.v1+json",
                "application/vnd.docker.distribution.manifest.list.v2+json",
                "application/vnd.docker.distribution.manifest.v2+json",
            )
        ),
        "User-Agent": "Azure-taugrid-release-sbom",
    }
    with registry_request(url, headers) as response:
        body = response.read()
        digest = response.headers.get("Docker-Content-Digest", "")
    if not digest:
        digest = "sha256:" + hashlib.sha256(body).hexdigest()
    if not SHA256_RE.fullmatch(digest):
        fail(f"registry returned invalid digest {digest!r} for {reference}")
    try:
        manifest = json.loads(body)
    except json.JSONDecodeError as exc:
        fail(f"registry returned invalid manifest JSON for {reference}: {exc}")
    descriptors = manifest.get("manifests")
    if not isinstance(descriptors, list):
        fail(f"{reference} did not resolve to a multi-architecture image index")

    platforms: dict[str, dict[str, str]] = {}
    for descriptor in descriptors:
        if not isinstance(descriptor, dict):
            continue
        platform_data = descriptor.get("platform")
        if not isinstance(platform_data, dict):
            continue
        platform = f"{platform_data.get('os', '')}/{platform_data.get('architecture', '')}"
        if platform not in IMAGE_PLATFORMS:
            continue
        platform_digest = str(descriptor.get("digest", ""))
        if not SHA256_RE.fullmatch(platform_digest):
            fail(f"{reference} has invalid {platform} manifest digest {platform_digest!r}")
        if platform in platforms:
            fail(f"{reference} has duplicate {platform} manifests")
        platforms[platform] = {
            "platform": platform,
            "platformDigest": platform_digest,
            "platformImage": f"{repository}@{platform_digest}",
        }
    if set(platforms) != set(IMAGE_PLATFORMS):
        fail(
            f"{reference} platform coverage must be exactly {list(IMAGE_PLATFORMS)}, "
            f"got {sorted(platforms)}"
        )
    return {
        "imageIndex": f"{repository}@{digest}",
        "imageIndexDigest": digest,
        "platforms": [platforms[platform] for platform in IMAGE_PLATFORMS],
    }


def resolve_images(input_path: Path, output_path: Path, attempts: int, delay: int) -> None:
    specs = read_json(input_path)
    if not isinstance(specs, list):
        fail("image metadata must be a JSON array")
    expected = {spec[0]: spec[1] for spec in IMAGE_SPECS}
    seen: set[str] = set()
    for spec in specs:
        if not isinstance(spec, dict):
            fail("each tagged image entry must be an object")
        required = {
            "component",
            "chart",
            "chartVersion",
            "componentVersion",
            "tagReference",
        }
        missing = required - spec.keys()
        if missing:
            fail(f"tagged image entry is missing {sorted(missing)}")
        component = str(spec["component"])
        if component in seen:
            fail(f"duplicate tagged image component {component}")
        seen.add(component)
        if spec["chart"] != expected.get(component):
            fail(f"{component} has unexpected chart {spec['chart']!r}")
        if spec["componentVersion"] != spec["chartVersion"]:
            fail(f"{component} component and chart versions do not match")
        if "@" in str(spec["tagReference"]):
            fail(f"{component} input must be a tag reference before resolution")
    if seen != set(expected):
        fail(
            f"tagged image components must be exactly {sorted(expected)}, "
            f"got {sorted(seen)}"
        )
    resolved = []
    for spec in specs:
        last_error: Exception | None = None
        for attempt in range(1, attempts + 1):
            try:
                item = dict(spec)
                item.update(resolve_image(item["tagReference"]))
                resolved.append(item)
                print(f"resolved {item['tagReference']} to {item['imageIndex']}")
                break
            except (OSError, ValueError, SbomError) as exc:
                last_error = exc
                if attempt == attempts:
                    break
                print(
                    f"image {spec.get('tagReference')} is not visible yet "
                    f"(attempt {attempt}/{attempts}): {exc}",
                    file=sys.stderr,
                )
                time.sleep(delay)
        else:
            last_error = SbomError("image resolution exhausted attempts")
        if len(resolved) == 0 or resolved[-1].get("component") != spec.get("component"):
            fail(
                f"cannot resolve published image {spec.get('tagReference')} "
                f"after {attempts} attempts: {last_error}"
            )
    write_json(output_path, resolved)


def validate_spdx(path: Path) -> dict[str, Any]:
    document = read_json(path)
    if not isinstance(document, dict):
        fail(f"{path} must contain a JSON object")
    if document.get("SPDXID") != "SPDXRef-DOCUMENT":
        fail(f"{path} has invalid SPDXID")
    version = document.get("spdxVersion")
    if not isinstance(version, str) or not version.startswith("SPDX-2."):
        fail(f"{path} is not an SPDX 2.x document")
    if document.get("dataLicense") != "CC0-1.0":
        fail(f"{path} has invalid dataLicense")
    if not document.get("documentNamespace"):
        fail(f"{path} has no documentNamespace")
    creation = document.get("creationInfo")
    if (
        not isinstance(creation, dict)
        or not valid_timestamp(creation.get("created"))
        or not creation.get("creators")
    ):
        fail(f"{path} has incomplete creationInfo")
    if not document.get("packages") and not document.get("files"):
        fail(f"{path} describes no packages or files")
    describes = document.get("documentDescribes", [])
    relationships = document.get("relationships", [])
    has_describes = bool(describes) or any(
        relationship.get("spdxElementId") == "SPDXRef-DOCUMENT"
        and relationship.get("relationshipType") == "DESCRIBES"
        for relationship in relationships
        if isinstance(relationship, dict)
    )
    if not has_describes:
        fail(f"{path} has no document DESCRIBES relationship")
    return document


def sbom_namespace(release_tag: str, sbom_name: str) -> str:
    return (
        f"https://github.com/Azure/taugrid/sbom/{release_tag}/"
        f"{urllib.parse.quote(sbom_name, safe='')}"
    )


def cli_sbom_name(asset: str) -> str:
    return f"{asset}.spdx.json"


def image_source_name(component: str, platform: str) -> str:
    return f"{component}-image-{platform.replace('/', '-')}"


def image_sbom_name(
    component: str, component_version: str, platform: str, platform_digest: str
) -> str:
    return (
        f"{component}-image-{component_version}-{platform.replace('/', '-')}-"
        f"{platform_digest.replace(':', '-')}.spdx.json"
    )


def validate_spdx_identity(
    path: Path, expected_name: str, expected_namespace: str
) -> dict[str, Any]:
    document = validate_spdx(path)
    if document.get("name") != expected_name:
        fail(
            f"{path} SPDX name {document.get('name')!r} does not match "
            f"{expected_name!r}"
        )
    if document.get("documentNamespace") != expected_namespace:
        fail(f"{path} has an unexpected documentNamespace")
    return document


def run_syft(
    syft: Path,
    source: str,
    output: Path,
    generated_at: str,
    namespace: str,
    source_name: str,
    source_version: str,
    platform: str | None = None,
) -> None:
    command = [
        str(syft),
        "scan",
        source,
        "--source-name",
        source_name,
        "--source-version",
        source_version,
    ]
    if platform is not None:
        command.extend(("--platform", platform))
    command.extend(("--output", f"spdx-json={output}"))
    try:
        subprocess.run(command, check=True)
    except (OSError, subprocess.CalledProcessError) as exc:
        fail(f"Syft failed for {source}: {exc}")
    document = validate_spdx(output)
    if document.get("name") != source_name:
        fail(f"Syft SPDX name for {source} does not match {source_name!r}")
    document["documentNamespace"] = namespace
    document["creationInfo"]["created"] = generated_at
    write_json(output, document)
    validate_spdx_identity(output, source_name, namespace)


def validate_image_metadata(value: Any) -> list[dict[str, Any]]:
    if not isinstance(value, list):
        fail("resolved image metadata must be a JSON array")
    expected = {spec[0] for spec in IMAGE_SPECS}
    expected_charts = {spec[0]: spec[1] for spec in IMAGE_SPECS}
    seen: set[str] = set()
    result: list[dict[str, Any]] = []
    for raw in value:
        if not isinstance(raw, dict):
            fail("each resolved image entry must be an object")
        required = {
            "component",
            "chart",
            "chartVersion",
            "componentVersion",
            "tagReference",
            "imageIndex",
            "imageIndexDigest",
            "platforms",
        }
        missing = required - raw.keys()
        if missing:
            fail(f"resolved image entry is missing {sorted(missing)}")
        component = str(raw["component"])
        if component in seen:
            fail(f"duplicate resolved image component {component}")
        seen.add(component)
        if str(raw["chart"]) != expected_charts.get(component):
            fail(f"{component} has unexpected chart {raw['chart']!r}")
        if str(raw["componentVersion"]) != str(raw["chartVersion"]):
            fail(f"{component} component and chart versions do not match")
        image_index = str(raw["imageIndex"])
        image_index_digest = str(raw["imageIndexDigest"])
        if not SHA256_RE.fullmatch(image_index_digest):
            fail(f"{component} has invalid image index digest {image_index_digest!r}")
        if not image_index.endswith(f"@{image_index_digest}"):
            fail(f"{component} image index reference and digest do not match")
        repository = image_index.split("@", 1)[0]
        if str(raw["tagReference"]).rsplit(":", 1)[0] != repository:
            fail(f"{component} tag and digest references use different repositories")
        platform_values = raw["platforms"]
        if not isinstance(platform_values, list):
            fail(f"{component} platforms must be an array")
        platforms: dict[str, dict[str, str]] = {}
        for platform_value in platform_values:
            if not isinstance(platform_value, dict):
                fail(f"{component} platform entries must be objects")
            platform = str(platform_value.get("platform", ""))
            platform_digest = str(platform_value.get("platformDigest", ""))
            platform_image = str(platform_value.get("platformImage", ""))
            if platform in platforms:
                fail(f"{component} has duplicate platform {platform}")
            if platform not in IMAGE_PLATFORMS:
                fail(f"{component} has unsupported platform {platform!r}")
            if not SHA256_RE.fullmatch(platform_digest):
                fail(f"{component} {platform} has invalid platform digest")
            if platform_image != f"{repository}@{platform_digest}":
                fail(f"{component} {platform} reference and digest do not match")
            platforms[platform] = {
                "platform": platform,
                "platformDigest": platform_digest,
                "platformImage": platform_image,
            }
        if set(platforms) != set(IMAGE_PLATFORMS):
            fail(
                f"{component} platform coverage must be exactly "
                f"{list(IMAGE_PLATFORMS)}, got {sorted(platforms)}"
            )
        result.append(
            {
                "component": component,
                "chart": str(raw["chart"]),
                "chartVersion": str(raw["chartVersion"]),
                "componentVersion": str(raw["componentVersion"]),
                "tagReference": str(raw["tagReference"]),
                "imageIndex": image_index,
                "imageIndexDigest": image_index_digest,
                "platforms": [platforms[platform] for platform in IMAGE_PLATFORMS],
            }
        )
    if seen != expected:
        fail(f"resolved image components must be exactly {sorted(expected)}, got {sorted(seen)}")
    return result


def generate(args: argparse.Namespace) -> None:
    release_dir = args.release_dir.resolve()
    if not release_dir.is_dir():
        fail(f"release directory does not exist: {release_dir}")
    images = validate_image_metadata(read_json(args.images))
    version = args.release_tag
    if not version.startswith("v") or not SEMVER_RE.fullmatch(version[1:]):
        fail(f"release tag must be v-prefixed SemVer, got {version!r}")
    coordinated_version = version[1:]
    for image in images:
        if image["componentVersion"] != coordinated_version:
            fail(
                f"{image['component']} version {image['componentVersion']} does not "
                f"match release tag {version}"
            )
    if not re.fullmatch(r"[0-9a-f]{40}", args.source_commit):
        fail("source commit must be a full 40-character lowercase SHA")

    cli_entries = []
    for component, target, asset in CLI_TARGETS:
        asset_path = release_dir / asset
        if not asset_path.is_file():
            fail(f"missing release binary {asset}")
        sbom_name = cli_sbom_name(asset)
        sbom_path = release_dir / sbom_name
        namespace = sbom_namespace(version, sbom_name)
        run_syft(
            args.syft,
            f"file:{asset_path}",
            sbom_path,
            args.generated_at,
            namespace,
            asset,
            version,
        )
        cli_entries.append(
            {
                "component": component,
                "target": target,
                "version": version,
                "asset": asset,
                "assetSha256": sha256(asset_path),
                "sbom": sbom_name,
                "sbomSha256": sha256(sbom_path),
            }
        )

    image_entries = []
    for image in sorted(images, key=lambda item: item["component"]):
        for platform_data in image["platforms"]:
            platform = platform_data["platform"]
            platform_digest = platform_data["platformDigest"]
            sbom_name = image_sbom_name(
                image["component"],
                image["componentVersion"],
                platform,
                platform_digest,
            )
            sbom_path = release_dir / sbom_name
            namespace = sbom_namespace(version, sbom_name)
            source_name = image_source_name(image["component"], platform)
            run_syft(
                args.syft,
                f"registry:{platform_data['platformImage']}",
                sbom_path,
                args.generated_at,
                namespace,
                source_name,
                image["componentVersion"],
                platform,
            )
            image_entries.append(
                {
                    "component": image["component"],
                    "chart": image["chart"],
                    "chartVersion": image["chartVersion"],
                    "componentVersion": image["componentVersion"],
                    "tagReference": image["tagReference"],
                    "imageIndex": image["imageIndex"],
                    "imageIndexDigest": image["imageIndexDigest"],
                    "platform": platform,
                    "platformImage": platform_data["platformImage"],
                    "platformDigest": platform_digest,
                    "sbom": sbom_name,
                    "sbomSha256": sha256(sbom_path),
                }
            )

    index = {
        "schemaVersion": SCHEMA_VERSION,
        "release": {
            "tag": version,
            "sourceCommit": args.source_commit,
            "generatedAt": args.generated_at,
        },
        "cli": cli_entries,
        "images": image_entries,
    }
    write_json(release_dir / INDEX_FILENAME, index)
    write_checksums(release_dir)
    validate_release(release_dir)


def write_checksums(release_dir: Path) -> None:
    assets = sorted(
        path
        for path in release_dir.iterdir()
        if path.is_file() and path.name != "SHA256SUMS"
    )
    if not assets:
        fail(f"release directory is empty: {release_dir}")
    lines = [f"{sha256(path)}  {path.name}\n" for path in assets]
    (release_dir / "SHA256SUMS").write_text("".join(lines), encoding="utf-8")


def checksum_entries(release_dir: Path) -> dict[str, str]:
    path = release_dir / "SHA256SUMS"
    if not path.is_file():
        fail(f"release is missing {path.name}")
    entries: dict[str, str] = {}
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        match = re.fullmatch(r"([0-9a-f]{64})  ([^/]+)", line)
        if not match:
            fail(f"{path}:{number} is not a valid SHA256SUMS entry")
        digest, name = match.groups()
        if name in entries:
            fail(f"{path} has duplicate checksum entry for {name}")
        entries[name] = digest
    return entries


def expected_assets(release_dir: Path, index: dict[str, Any]) -> set[str]:
    wheel_names = [
        path.name
        for path in release_dir.glob("tau-*.whl")
        if path.is_file()
    ]
    if len(wheel_names) != 1:
        fail("release must contain exactly one tau-*.whl asset")
    base = {
        "LICENSE",
        "SHA256SUMS",
        "install.sh",
        "install.ps1",
        wheel_names[0],
        INDEX_FILENAME,
    }
    base.update(asset for _, _, asset in CLI_TARGETS)
    base.update(entry["sbom"] for entry in index["cli"])
    base.update(entry["sbom"] for entry in index["images"])
    return base


def validate_index(release_dir: Path) -> dict[str, Any]:
    index = read_json(release_dir / INDEX_FILENAME)
    if not isinstance(index, dict) or index.get("schemaVersion") != SCHEMA_VERSION:
        fail(f"{INDEX_FILENAME} has unsupported schemaVersion")
    release = index.get("release")
    if (
        not isinstance(release, dict)
        or not str(release.get("tag", "")).startswith("v")
        or not SEMVER_RE.fullmatch(str(release.get("tag", ""))[1:])
        or not re.fullmatch(r"[0-9a-f]{40}", str(release.get("sourceCommit", "")))
        or not valid_timestamp(release.get("generatedAt"))
    ):
        fail(f"{INDEX_FILENAME} has invalid release metadata")

    cli = index.get("cli")
    if not isinstance(cli, list):
        fail(f"{INDEX_FILENAME} cli must be an array")
    expected_cli = {(component, target, asset) for component, target, asset in CLI_TARGETS}
    actual_cli: set[tuple[str, str, str]] = set()
    sbom_names: set[str] = set()
    for entry in cli:
        if not isinstance(entry, dict):
            fail(f"{INDEX_FILENAME} CLI entries must be objects")
        key = (entry.get("component"), entry.get("target"), entry.get("asset"))
        if key in actual_cli:
            fail(f"{INDEX_FILENAME} has duplicate CLI entry {key}")
        actual_cli.add(key)
        if entry.get("version") != release["tag"]:
            fail(f"{INDEX_FILENAME} CLI entry {key} has the wrong version")
        expected_sbom = cli_sbom_name(str(entry.get("asset", "")))
        if entry.get("sbom") != expected_sbom:
            fail(f"{INDEX_FILENAME} CLI entry {key} has unexpected SBOM filename")
        asset = release_dir / str(entry.get("asset", ""))
        sbom = release_dir / str(entry.get("sbom", ""))
        if not asset.is_file() or sha256(asset) != entry.get("assetSha256"):
            fail(f"{INDEX_FILENAME} CLI asset digest mismatch for {key}")
        if not sbom.is_file() or sha256(sbom) != entry.get("sbomSha256"):
            fail(f"{INDEX_FILENAME} CLI SBOM digest mismatch for {key}")
        validate_spdx_identity(
            sbom,
            str(entry.get("asset", "")),
            sbom_namespace(str(release["tag"]), expected_sbom),
        )
        if sbom.name in sbom_names:
            fail(f"{INDEX_FILENAME} reuses SBOM filename {sbom.name}")
        sbom_names.add(sbom.name)
    if actual_cli != expected_cli:
        fail("release index does not contain exactly every tau and tau-gen target")

    images = index.get("images")
    if not isinstance(images, list):
        fail(f"{INDEX_FILENAME} images must be an array")
    expected_images = {
        (spec[0], platform) for spec in IMAGE_SPECS for platform in IMAGE_PLATFORMS
    }
    expected_charts = {spec[0]: spec[1] for spec in IMAGE_SPECS}
    actual_images: set[tuple[str, str]] = set()
    image_indexes: dict[str, tuple[str, str, str]] = {}
    for entry in images:
        if not isinstance(entry, dict):
            fail(f"{INDEX_FILENAME} image entries must be objects")
        component = str(entry.get("component", ""))
        platform = str(entry.get("platform", ""))
        key = (component, platform)
        if key in actual_images:
            fail(f"{INDEX_FILENAME} has duplicate image entry {key}")
        actual_images.add(key)
        if platform not in IMAGE_PLATFORMS:
            fail(f"{INDEX_FILENAME} image {component} has unsupported platform {platform}")
        if entry.get("chart") != expected_charts.get(component):
            fail(f"{INDEX_FILENAME} image {component} has the wrong chart")
        if entry.get("componentVersion") != entry.get("chartVersion"):
            fail(f"{INDEX_FILENAME} image {component} has inconsistent versions")
        if entry.get("componentVersion") != str(release["tag"])[1:]:
            fail(f"{INDEX_FILENAME} image {component} does not match the release tag")
        image_index = str(entry.get("imageIndex", ""))
        image_index_digest = str(entry.get("imageIndexDigest", ""))
        if not SHA256_RE.fullmatch(image_index_digest):
            fail(f"{INDEX_FILENAME} image {component} has invalid index digest")
        if not image_index.endswith(f"@{image_index_digest}"):
            fail(f"{INDEX_FILENAME} image {component} index reference does not match")
        repository = image_index.split("@", 1)[0]
        tag_reference = str(entry.get("tagReference", ""))
        if tag_reference.rsplit(":", 1)[0] != repository:
            fail(f"{INDEX_FILENAME} image {component} has inconsistent repositories")
        platform_digest = str(entry.get("platformDigest", ""))
        platform_image = str(entry.get("platformImage", ""))
        if not SHA256_RE.fullmatch(platform_digest):
            fail(f"{INDEX_FILENAME} image {key} has invalid platform digest")
        if platform_image != f"{repository}@{platform_digest}":
            fail(f"{INDEX_FILENAME} image {key} platform reference does not match")
        index_identity = (
            tag_reference,
            image_index,
            image_index_digest,
        )
        if component in image_indexes and image_indexes[component] != index_identity:
            fail(f"{INDEX_FILENAME} image {component} platforms disagree on image index")
        image_indexes[component] = index_identity
        expected_sbom = image_sbom_name(
            component,
            str(entry.get("componentVersion", "")),
            platform,
            platform_digest,
        )
        if entry.get("sbom") != expected_sbom:
            fail(f"{INDEX_FILENAME} image {key} has unexpected SBOM filename")
        sbom = release_dir / str(entry.get("sbom", ""))
        if not sbom.is_file() or sha256(sbom) != entry.get("sbomSha256"):
            fail(f"{INDEX_FILENAME} image SBOM digest mismatch for {key}")
        validate_spdx_identity(
            sbom,
            image_source_name(component, platform),
            sbom_namespace(str(release["tag"]), expected_sbom),
        )
        if sbom.name in sbom_names:
            fail(f"{INDEX_FILENAME} reuses SBOM filename {sbom.name}")
        sbom_names.add(sbom.name)
    if actual_images != expected_images:
        fail(
            "release index image platform coverage must be exactly "
            f"{sorted(expected_images)}, got {sorted(actual_images)}"
        )
    return index


def validate_release(release_dir: Path) -> None:
    index = validate_index(release_dir)
    expected = expected_assets(release_dir, index)
    actual = {path.name for path in release_dir.iterdir() if path.is_file()}
    if actual != expected:
        fail(
            f"release asset set mismatch; missing={sorted(expected - actual)}, "
            f"unexpected={sorted(actual - expected)}"
        )
    checksums = checksum_entries(release_dir)
    checksummed = expected - {"SHA256SUMS"}
    if set(checksums) != checksummed:
        fail(
            "SHA256SUMS asset set mismatch; "
            f"missing={sorted(checksummed - set(checksums))}, "
            f"unexpected={sorted(set(checksums) - checksummed)}"
        )
    for name, expected_digest in checksums.items():
        if sha256(release_dir / name) != expected_digest:
            fail(f"checksum mismatch for {name}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)

    specs = subparsers.add_parser("image-specs")
    specs.add_argument("--repo-root", type=Path, default=Path.cwd())
    specs.add_argument("--output", type=Path, required=True)

    resolve = subparsers.add_parser("resolve-images")
    resolve.add_argument("--input", type=Path, required=True)
    resolve.add_argument("--output", type=Path, required=True)
    resolve.add_argument("--attempts", type=int, default=1)
    resolve.add_argument("--delay", type=int, default=0)

    generate_parser = subparsers.add_parser("generate")
    generate_parser.add_argument("--syft", type=Path, required=True)
    generate_parser.add_argument("--release-dir", type=Path, required=True)
    generate_parser.add_argument("--images", type=Path, required=True)
    generate_parser.add_argument("--release-tag", required=True)
    generate_parser.add_argument("--source-commit", required=True)
    generate_parser.add_argument("--generated-at", required=True)

    validate_parser = subparsers.add_parser("validate")
    validate_parser.add_argument("--release-dir", type=Path, required=True)

    expected_parser = subparsers.add_parser("expected-assets")
    expected_parser.add_argument("--release-dir", type=Path, required=True)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    try:
        if args.command == "image-specs":
            write_json(args.output, image_specs(args.repo_root.resolve()))
        elif args.command == "resolve-images":
            if args.attempts < 1 or args.delay < 0:
                fail("attempts must be positive and delay must be non-negative")
            resolve_images(args.input, args.output, args.attempts, args.delay)
        elif args.command == "generate":
            generate(args)
        elif args.command == "validate":
            validate_release(args.release_dir.resolve())
        elif args.command == "expected-assets":
            release_dir = args.release_dir.resolve()
            index = validate_index(release_dir)
            print("\n".join(sorted(expected_assets(release_dir, index))))
        return 0
    except SbomError as exc:
        print(f"release SBOM error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
