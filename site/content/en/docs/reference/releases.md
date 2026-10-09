---
title: Release contract
weight: 4
description: How TauGrid binaries, SDK releases, and SBOM assets are published
---

{{< maturity status="ga" reviewed="2026-10-08" >}}

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
3. Requires checked-in curated release notes and appends GitHub-generated
   change attribution, new-contributor recognition, and a full changelog link.
4. Runs Go and Python release gates.
5. Compares two independent binary builds.
6. Builds the Python SDK wheel twice and requires byte-for-byte identical
   output.
7. Resolves the public `tau`, `taugrid-portal`, and `tau-core-controller`
   image tags to immutable MCR image-index digests, then resolves their Linux
   amd64 and arm64 platform manifest digests and runs pinned Syft against each
   exact platform image.
8. Generates SPDX JSON SBOMs for every published `tau` and `tau-gen` platform
   binary and both Linux platforms of the three coordinated images.
9. Publishes raw binaries, the SDK wheel, installers, `LICENSE`, the SBOMs,
   `taugrid-release-sbom-index.json`, and `SHA256SUMS` as new assets, leaving
   any existing ones untouched.
10. Validates the SPDX documents, index completeness, asset set, and every
    uploaded digest.
11. Proves CLI and SDK installation on clean Ubuntu, macOS, and Windows runners.

The Python SDK keeps its own package version, but its source-aligned
`tau-*.whl` is published in the same GitHub Release as the CLI.

The release index maps CLI targets and chart/component versions to exact SBOM
assets and checksums. Image entries preserve the versioned tag and top-level
OCI image-index digest, plus the platform and immutable platform manifest
digest for each SBOM. These GitHub Release SBOM assets are not OCI referrers,
attestations, or signatures.

See
[`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md).
