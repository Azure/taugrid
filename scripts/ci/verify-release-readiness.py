#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request

from release_artifacts import (
    CHARTS,
    IMAGES,
    REVISION,
    STABLE,
    ReleaseError,
    git,
    metadata,
    verify_chart,
)


class MissingArtifact(ReleaseError):
    pass


class IncompleteRelease(ReleaseError):
    pass


RELEASE_ASSETS = (
    "LICENSE",
    "SHA256SUMS",
    "install.ps1",
    "install.sh",
    "tau-darwin-amd64",
    "tau-darwin-arm64",
    "tau-gen-darwin-amd64",
    "tau-gen-darwin-arm64",
    "tau-gen-linux-amd64",
    "tau-gen-linux-arm64",
    "tau-gen-windows-amd64.exe",
    "tau-linux-amd64",
    "tau-linux-arm64",
    "tau-windows-amd64.exe",
)
SDK_VERSION = (
    r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:[A-Za-z0-9.]+)?"
)
WHEEL = re.compile(rf"tau-({SDK_VERSION})-py3-none-any\.whl")


def sdk_wheel(commit):
    source = git("show", f"{commit}:sdk/python/python/tau/__init__.py")
    matches = re.findall(
        rf'^__version__\s*=\s*"({SDK_VERSION})"\s*$',
        source,
        flags=re.MULTILINE,
    )
    if len(matches) != 1:
        raise ReleaseError("tagged source must define one stable Tau SDK version")
    return f"tau-{matches[0]}-py3-none-any.whl"


class HTTP:
    def __init__(self, deadline):
        self.deadline = deadline

    def get(
        self,
        url,
        *,
        token=None,
        missing=False,
        accept=None,
        max_size=64 * 1024 * 1024,
    ):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise ReleaseError("release readiness deadline exceeded")
        if accept is None:
            accept = ", ".join(
                (
                    "application/vnd.oci.image.index.v1+json",
                    "application/vnd.docker.distribution.manifest.list.v2+json",
                    "application/vnd.oci.image.manifest.v1+json",
                    "application/vnd.docker.distribution.manifest.v2+json",
                )
            )
            if urllib.parse.urlparse(url).hostname == "api.github.com":
                accept = "application/vnd.github+json"
        headers = {"Accept": accept}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        request = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(
                request, timeout=min(20, remaining)
            ) as response:
                chunks = []
                size = 0
                while True:
                    if time.monotonic() >= self.deadline:
                        raise ReleaseError("release readiness deadline exceeded")
                    chunk = response.read1(64 * 1024)
                    if not chunk:
                        return b"".join(chunks)
                    size += len(chunk)
                    if size > max_size:
                        raise ReleaseError(f"response exceeds size limit: {url}")
                    chunks.append(chunk)
        except urllib.error.HTTPError as error:
            error.close()
            if missing and error.code == 404:
                raise MissingArtifact(f"not yet available: {url}") from error
            raise ReleaseError(f"HTTP {error.code} reading {url}") from error
        except (urllib.error.URLError, TimeoutError, socket.timeout) as error:
            raise ReleaseError(f"network error reading {url}: {error}") from error


class GitHub:
    def __init__(self, http, repository, token):
        self.http = http
        self.repository = repository
        self.token = token
        self.base = f"https://api.github.com/repos/{repository}"

    def get(self, path):
        return json.loads(self.http.get(f"{self.base}/{path}", token=self.token))

    def tag(self, tag):
        if not re.fullmatch("v" + STABLE, tag):
            raise ReleaseError("only canonical stable vX.Y.Z tags are eligible")
        ref = self.get(f"git/ref/tags/{tag}")["object"]
        if ref["type"] != "tag":
            raise ReleaseError("release tag must be annotated")
        annotated = self.get(f"git/tags/{ref['sha']}")
        commit = annotated["object"]
        if annotated["tag"] != tag or commit["type"] != "commit":
            raise ReleaseError("annotated tag must directly name its release commit")
        if not re.fullmatch(r"[0-9a-f]{40}", commit["sha"]):
            raise ReleaseError("release tag returned an invalid source SHA")
        return ref["sha"], commit["sha"]

    def release(self, tag, wheel):
        if not WHEEL.fullmatch(wheel):
            raise ReleaseError("invalid expected Tau SDK wheel name")
        release = self.get(f"releases/tags/{tag}")
        if (
            release["tag_name"] != tag
            or release["draft"]
            or release["prerelease"]
            or not release["published_at"]
        ):
            raise ReleaseError("a published stable GitHub release is required")
        assets = release["assets"]
        names = [asset["name"] for asset in assets]
        if len(names) != len(set(names)):
            raise ReleaseError("GitHub release contains duplicate asset names")
        expected = set(RELEASE_ASSETS) | {wheel}
        if set(names) != expected:
            raise ReleaseError("GitHub release asset set does not match the contract")
        by_name = {asset["name"]: asset for asset in assets}
        for asset in assets:
            expected_url = (
                f"https://github.com/{self.repository}/releases/download/"
                f"{urllib.parse.quote(tag, safe='')}/"
                f"{urllib.parse.quote(asset['name'], safe='')}"
            )
            if (
                asset["state"] != "uploaded"
                or not isinstance(asset["size"], int)
                or asset["size"] <= 0
                or asset.get("browser_download_url") != expected_url
                or not re.fullmatch(
                    r"sha256:[0-9a-f]{64}", str(asset.get("digest", ""))
                )
            ):
                raise ReleaseError(f"invalid GitHub release asset: {asset['name']}")
        checksum_asset = by_name["SHA256SUMS"]
        checksum_bytes = self.http.get(
            checksum_asset["browser_download_url"],
            accept="application/octet-stream",
            max_size=1024 * 1024,
        )
        if len(checksum_bytes) != checksum_asset["size"]:
            raise ReleaseError("SHA256SUMS GitHub asset size mismatch")
        checksum_digest = f"sha256:{hashlib.sha256(checksum_bytes).hexdigest()}"
        if checksum_asset["digest"] != checksum_digest:
            raise ReleaseError("SHA256SUMS GitHub asset digest mismatch")
        try:
            lines = checksum_bytes.decode("utf-8").splitlines()
        except UnicodeDecodeError as error:
            raise ReleaseError("SHA256SUMS must be UTF-8") from error
        checksums = {}
        for line in lines:
            match = re.fullmatch(
                r"([0-9a-f]{64})  ([A-Za-z0-9][A-Za-z0-9._+-]*)", line
            )
            if not match or match[2] in checksums:
                raise ReleaseError("invalid SHA256SUMS entry")
            checksums[match[2]] = match[1]
        expected_checksums = expected - {"SHA256SUMS"}
        if set(checksums) != expected_checksums:
            raise ReleaseError("SHA256SUMS asset set does not match the release")
        for name in expected_checksums:
            if by_name[name]["digest"] != f"sha256:{checksums[name]}":
                raise ReleaseError(f"GitHub asset checksum mismatch: {name}")
        return release

    def resolve(self, tag):
        identity = self.tag(tag)
        return tag, identity


class Registry:
    def __init__(self, http):
        self.http = http
        self.base = "https://mcr.microsoft.com/v2/aks/ai-runtime"

    def manifest(self, repository, reference):
        return json.loads(
            self.http.get(
                f"{self.base}/{repository}/manifests/{reference}",
                missing=True,
            )
        )

    def blob(self, repository, digest):
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise ReleaseError("invalid registry blob digest")
        data = self.http.get(f"{self.base}/{repository}/blobs/{digest}", missing=True)
        if f"sha256:{hashlib.sha256(data).hexdigest()}" != digest:
            raise ReleaseError("registry blob digest mismatch")
        return data

    def image(self, name, release):
        manifest = self.manifest(name, release["version"])
        if manifest.get("annotations", {}).get(REVISION) != release["commit"]:
            raise ReleaseError(
                f"image index revision mismatch or missing annotation: {name}"
            )
        platforms = {
            (
                entry.get("platform", {}).get("os"),
                entry.get("platform", {}).get("architecture"),
            )
            for entry in manifest.get("manifests", [])
        }
        if not {("linux", "amd64"), ("linux", "arm64")} <= platforms:
            raise ReleaseError(
                f"image must include linux/amd64 and linux/arm64: {name}"
            )

    def chart(self, name, release):
        repository = f"helm/{name}"
        manifest = self.manifest(repository, release["charts"][name]["version"])
        layers = [
            layer
            for layer in manifest["layers"]
            if layer["mediaType"]
            == "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
        ]
        if len(layers) != 1:
            raise ReleaseError(f"expected exactly one Helm chart layer: {name}")
        verify_chart(self.blob(repository, layers[0]["digest"]), name, release)


def check_ready(registry, release, identity_check):
    pending = []
    for kind, names in (("image", IMAGES), ("chart", CHARTS)):
        for name in names:
            try:
                getattr(registry, kind)(name, release)
            except MissingArtifact:
                pending.append(f"{kind}/{name}")
    identity_check()
    return pending


def wait_ready(
    registry,
    release,
    deadline,
    interval,
    identity_check,
    sleep=time.sleep,
    clock=time.monotonic,
):
    while True:
        pending = check_ready(registry, release, identity_check)
        if not pending:
            if clock() >= deadline:
                raise ReleaseError("release readiness deadline exceeded")
            return
        remaining = deadline - clock()
        if remaining <= 0:
            raise ReleaseError(
                f"release readiness timed out; missing: {', '.join(pending)}"
            )
        print(f"Waiting for: {', '.join(pending)}", flush=True)
        sleep(min(interval, remaining))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", default="")
    parser.add_argument("--version", default="")
    parser.add_argument("--timeout", type=int, default=5400)
    parser.add_argument("--interval", type=int, default=60)
    parser.add_argument("--check-once", action="store_true")
    parser.add_argument("--artifacts-only", action="store_true")
    args = parser.parse_args()
    if not 1 <= args.timeout <= 14400 or not 1 <= args.interval <= 300:
        parser.error(
            "timeout must be 1..14400 seconds; interval must be 1..300 seconds"
        )
    effective_timeout = min(args.timeout, 60) if args.check_once else args.timeout
    deadline = time.monotonic() + effective_timeout
    http = HTTP(deadline)
    github = GitHub(
        http,
        os.environ.get("GITHUB_REPOSITORY", "Azure/taugrid"),
        os.environ.get("GH_TOKEN"),
    )
    tag, identity = github.resolve(args.tag)
    commit = identity[1]
    if args.commit and args.commit != commit:
        raise ReleaseError(
            "requested source commit does not match annotated release tag"
        )
    if args.version and args.version != tag[1:]:
        raise ReleaseError("requested version does not match annotated release tag")
    if git("rev-parse", f"refs/tags/{tag}") != identity[0]:
        raise ReleaseError("local tag differs from verified remote tag")
    if git("merge-base", commit, "origin/main") != commit:
        raise ReleaseError("release source is not an ancestor of main")
    release = metadata(commit, tag[1:])

    def check_identity():
        if github.tag(tag) != identity:
            raise ReleaseError("release tag moved during readiness verification")

    registry = Registry(http)
    if args.check_once:
        pending = check_ready(registry, release, check_identity)
        if pending:
            raise IncompleteRelease(
                f"release not ready; missing: {', '.join(pending)}"
            )
    else:
        wait_ready(registry, release, deadline, args.interval, check_identity)
    if not args.artifacts_only:
        github.release(tag, sdk_wheel(commit))
        check_identity()
    if args.artifacts_only:
        conclusion = (
            "All four multi-platform images and all five Helm charts are publicly "
            "available with matching release provenance."
        )
        heading = "Release artifacts ready"
    else:
        conclusion = (
            "All four multi-platform images, all five Helm charts, and the published "
            "GitHub release assets and checksums match the release contract."
        )
        heading = "Release ready"
    summary = (
        f"## {heading}\n\nTag: `{tag}`\n\nSource: `{commit}`\n\n{conclusion}\n"
    )
    print(summary)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a") as stream:
            stream.write(summary)


def entrypoint():
    try:
        main()
    except IncompleteRelease as error:
        print(f"release readiness: {error}", file=sys.stderr)
        return 3
    except (
        ReleaseError,
        AttributeError,
        KeyError,
        TypeError,
        UnicodeError,
        ValueError,
        OSError,
        tarfile.TarError,
        subprocess.CalledProcessError,
    ) as error:
        print(f"release readiness: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(entrypoint())
