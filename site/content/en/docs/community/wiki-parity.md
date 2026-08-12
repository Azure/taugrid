---
title: Site and wiki parity
weight: 3
description: Governance for two separately maintained documentation surfaces
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

The Tau documentation site and GitHub Wiki are both maintained as complete
surfaces. They are edited separately, so semantic parity is a human ownership
responsibility rather than an automated synchronization claim.

`site/data/wiki-parity.toml` records:

- The maintained site page and wiki counterpart.
- The owning team.
- Current maturity.
- Last joint review date.

The documentation pipeline validates that mapped site pages exist and metadata
is current whenever the site changes. Its consolidated Azure DevOps pipeline
also runs the same parity and review-age checks on the Monday parity-only
schedule.

When user-facing behavior or maturity changes:

1. Update the versioned site page.
2. Update the corresponding wiki page.
3. Review both as one documentation change.
4. Refresh the parity manifest review date.

The checks detect stale ownership and missing mappings. They do not prove that
two independently edited pages say the same thing.
