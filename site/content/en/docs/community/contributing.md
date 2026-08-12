---
title: Contributing
weight: 1
description: Contributor expectations and validation depth
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Start with the repository
[`AGENTS.md`](https://github.com/Azure/taugrid/blob/main/cli/AGENTS.md)
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
is not proven solely by a utility unit test.
