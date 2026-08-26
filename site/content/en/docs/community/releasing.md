---
title: Releasing TauGrid
weight: 2
description: Human-authorized and reproducible release publication
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

TauGrid release preparation updates:

- SDK version.
- Installation documentation.
- `cli/releases/vX.Y.Z.md`.
- The reviewed source commit on `main`.

Maintainers then create an annotated SemVer tag and manually dispatch the release
workflow. The workflow builds twice, compares outputs, verifies checksums,
publishes immutable assets, and proves clean bootstrap.

Never overwrite an existing release or asset. Do not update downstream minimum
versions until post-publication Ubuntu and macOS verification succeeds.

Follow the exact
[`RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md)
checklist.
