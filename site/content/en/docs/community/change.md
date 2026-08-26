---
title: Make a compatible change
weight: 1
description: Choose the owning package and prove the changed contract
aliases:
  - "/docs/tasks/contributor/change/"
---

{{< maturity status="ga" reviewed="2026-08-17" >}}

1. Start from the user path and compatibility contract.
2. Keep Cobra command files focused on wiring, flags, validation, and output.
3. Put reusable behavior in the owning `internal/*` capability package.
4. Preserve inspectable, versioned durable formats.
5. Keep expstore authoritative and hosted analytics downstream.
6. Test behavior at the layer that owns it.

Run focused package tests while iterating. Before TauGrid-internal closeout:

```bash
cd cli
go test ./...
```

Manifest, runtime, telemetry, or Kubernetes lifecycle changes also require the
smallest safe end-to-end path that proves the external contract. Use the
[Kind smoke test](../kind-smoke-test/) when the changed contract belongs to the
portable Kubernetes path.
