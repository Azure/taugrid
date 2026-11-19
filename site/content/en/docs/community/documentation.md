---
title: Documentation maintenance
weight: 3
description: Maintain and validate the versioned documentation site
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

The versioned documentation site is the sole maintained source for TauGrid
guides and reference material. Update documentation alongside the source
change that affects it.

The GitHub documentation validation and Pages deployment pipelines use the
same contract as local `make -C site check`: build, links, content metadata,
theme, accessibility, and documented CLI commands remain blocking checks.

The separate **TauGrid Docs Review Age** workflow runs every Monday and on manual
dispatch. Run `make -C site check-review-age` locally for the same strict audit:
capability reviews expire after 120 days. These maintenance
deadlines do not block unrelated pull requests or site publication. Direct
checker invocations remain strict unless `--skip-review-age` is supplied;
malformed or future dates are always errors.

A capability's `reviewed` date records a technical review of the site page.
Advance it only after checking the documented behavior against its implementation,
not merely to clear the maintenance audit.

When user-facing behavior or maturity changes:

1. Update the affected site page, examples, and maturity status.
2. Verify the documented behavior against the implementation.
3. Record the actual technical review date.
4. Run `make -C site check`.
