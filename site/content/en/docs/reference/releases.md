---
title: Release contract
weight: 4
description: How Tau CLI binaries and source-aligned SDK releases are published
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

TauGrid uses canonical annotated SemVer tags (`vX.Y.Z`) and a manually authorized
GitHub Actions workflow. Publishing a release requires a manually authorized
workflow run beyond the tag push itself. A tag push never publishes the GitHub
Release.

Before dispatching the GitHub workflow, release operators publish and verify the
coordinated first-party images and charts through the public Azure DevOps
pipelines. This is intentionally operator-sequenced: the GitHub workflow does
not have external ADO credentials and cannot prove those artifacts are ready.

The workflow:

1. Requires a manual dispatch from the existing annotated tag ref.
2. Verifies the annotated tag and reviewed `main` commit.
3. Requires checked-in release notes.
4. Runs Go and Python release gates.
5. Compares two independent binary builds.
6. Builds the Python SDK wheel twice and requires byte-for-byte identical
   output.
7. Publishes raw binaries, the SDK wheel, `install.sh`, `install.ps1`, `LICENSE`, and
   `SHA256SUMS` as new assets, leaving any existing ones untouched.
8. Verifies every uploaded digest.
9. Proves CLI and SDK installation on clean Ubuntu, macOS, and Windows runners.

The Python SDK keeps its own package version, but its source-aligned
`tau-*.whl` is published in the same GitHub Release as the CLI.

See
[`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md).
