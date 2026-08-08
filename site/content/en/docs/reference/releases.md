---
title: Release contract
weight: 4
description: How Tau binaries and source-aligned SDK releases are published
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau uses canonical annotated SemVer tags (`vX.Y.Z`) and a manually authorized
GitHub Actions workflow. Pushing a tag alone does not publish a release.

The workflow:

1. Verifies the annotated tag and reviewed `main` commit.
2. Requires checked-in release notes.
3. Runs Go and Python release gates.
4. Compares two independent binary builds.
5. Publishes raw binaries, `install.sh`, `LICENSE`, and `SHA256SUMS` without
   overwriting assets.
6. Verifies every uploaded digest.
7. Proves bootstrap and native commands on clean Ubuntu and macOS runners.

The Python SDK is installed from the same tagged source revision; GitHub
Releases do not currently contain wheels.

See
[`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md).
