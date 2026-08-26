---
title: Release contract
weight: 4
description: How Tau CLI binaries and source-aligned SDK releases are published
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

TauGrid uses canonical annotated SemVer tags (`vX.Y.Z`) and a manually authorized
GitHub Actions workflow. Publishing a release requires a manually authorized
workflow run beyond the tag push itself.

The workflow:

1. Verifies the annotated tag and reviewed `main` commit.
2. Requires checked-in release notes.
3. Runs Go and Python release gates.
4. Compares two independent binary builds.
5. Builds the Python SDK wheel twice and requires byte-for-byte identical
   output.
6. Publishes raw binaries, the SDK wheel, `install.sh`, `LICENSE`, and
   `SHA256SUMS` as new assets, leaving any existing ones untouched.
7. Verifies every uploaded digest.
8. Proves CLI and SDK installation on clean Ubuntu and macOS runners.

The Python SDK keeps its own package version, but its source-aligned
`tau-*.whl` is published in the same GitHub Release as the CLI.

See
[`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md).
