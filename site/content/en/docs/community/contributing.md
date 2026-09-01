---
title: Contributing
weight: 1
description: Contributor expectations and validation depth
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Start with the repository
[`AGENTS.md`](https://github.com/Azure/taugrid/blob/main/AGENTS.md)
and
[`SDK_GUIDE.md`](https://github.com/Azure/taugrid/blob/main/cli/SDK_GUIDE.md).

Core principles:

- Start from the user path and compatibility contract.
- Keep commands thin and capability packages cohesive.
- Reuse existing helpers and dependencies.
- Keep durable local formats inspectable and versioned.
- Preserve expstore as local truth.
- Make retry, checkpoint, and partial-write behavior explicit.
- Add tests at the owning layer.

External contract changes need external evidence. A manifest or telemetry change
needs evidence beyond a utility unit test.

## Publish a short blog post

Use the blog archetype to create consistent front matter:

```bash
cd site
make deps
./.bin/hugo new content --kind blog blog/<post-slug>.md
```

Replace the generated title and description, set `draft: false`, and write the
post in `content/en/blog/`. Keep detailed procedures in the task-oriented
documentation and link to them from the post. Run `make check` before opening a
pull request.
