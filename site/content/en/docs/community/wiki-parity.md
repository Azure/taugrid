---
title: Site and wiki parity
weight: 3
description: Governance for two separately maintained documentation surfaces
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

The TauGrid documentation site and GitHub Wiki are both maintained as complete
surfaces. They are edited separately, so semantic parity is a human ownership
responsibility rather than an automated synchronization claim.

`site/data/wiki-parity.toml` records:

- The maintained site page and wiki counterpart.
- The owning team.
- Current maturity.
- Last joint review date.

The GitHub documentation validation and Pages deployment pipelines validate that
mapped site pages exist and parity metadata is structurally valid without
blocking on review age. The consolidated Azure DevOps pipeline additionally
enforces review age on the Monday parity-only schedule. Local `make -C site check`
also enforces the 90-day review-age limit by default.

When user-facing behavior or maturity changes:

1. Update the versioned site page.
2. Update the corresponding wiki page.
3. Review both as one documentation change.
4. Refresh the parity manifest review date.

The checks detect stale ownership and missing mappings; confirm that two
independently edited pages say the same thing by reviewing both together, as
in step 3 above.
