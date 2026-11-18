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
blocking on review age. Local `make -C site check` uses the same contract:
build, links, content metadata, parity structure, theme, accessibility, and
documented CLI commands remain blocking checks.

The separate **TauGrid Docs Review Age** workflow runs every Monday and on manual
dispatch. Run `make -C site check-review-age` locally for the same strict audit:
capability reviews expire after 120 days, and joint parity reviews and site-only
rationales after 90 days. Both audits run even if one fails. These maintenance
deadlines do not block unrelated pull requests or site publication. Direct
checker invocations remain strict unless `--skip-review-age` is supplied;
malformed or future dates are always errors.

A capability's `reviewed` date records a technical review of the site page;
the manifest's `reviewed` date records a joint review of both surfaces. A newer
site review emits an advisory to assess the wiki counterpart, not a failure:
timestamp ordering alone cannot establish semantic drift or prove joint review.
Never advance the manifest date without reviewing both pages, including when
the wiki is unavailable.

When user-facing behavior or maturity changes:

1. Update the versioned site page.
2. Update the corresponding wiki page.
3. Review both as one documentation change.
4. Refresh the parity manifest review date.

The checks detect stale ownership and missing mappings; confirm that two
independently edited pages say the same thing by reviewing both together, as
in step 3 above.
