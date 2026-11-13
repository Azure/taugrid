#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import re
import subprocess
from pathlib import Path

SHA = re.compile(r"[0-9a-f]{40}")
TAG = re.compile(
    r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    r"(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?"
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def git(source, *arguments):
    try:
        return subprocess.run(
            ("git", "-C", str(source), *arguments),
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
    except subprocess.CalledProcessError as error:
        message = error.stderr.strip() or error.stdout.strip() or "git command failed"
        raise ValueError(message) from None


def validate(source, tag, source_commit):
    source = source.resolve()
    require(source.is_dir(), "release source directory does not exist")
    require(TAG.fullmatch(tag) is not None and len(tag) <= 128,
            "release tag must be v-prefixed SemVer without build metadata")
    require(SHA.fullmatch(source_commit) is not None,
            "release source must be a full immutable SHA")
    git(source, "fetch", "--force", "origin",
        "+refs/heads/main:refs/remotes/origin/main",
        f"+refs/tags/{tag}:refs/tags/{tag}")
    require(git(source, "cat-file", "-t", f"refs/tags/{tag}") == "tag",
            f"{tag} must be an annotated tag")
    tag_commit = git(source, "rev-parse", f"{tag}^{{commit}}")
    checkout_commit = git(source, "rev-parse", "HEAD^{commit}")
    require(tag_commit == source_commit,
            f"{tag} points to {tag_commit}, not {source_commit}")
    require(checkout_commit == source_commit,
            f"release checkout is {checkout_commit}, not {source_commit}")
    try:
        git(source, "merge-base", "--is-ancestor", source_commit, "origin/main")
    except ValueError:
        raise ValueError(f"{tag} points to {source_commit}, which is not on main") from None
    return tag_commit


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source-commit", required=True)
    args = parser.parse_args()
    validate(args.source, args.tag, args.source_commit)


if __name__ == "__main__":
    main()
