---
title: Make a compatible change
weight: 1
description: Choose the owning package and prove the changed contract
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

1. Start from the user path and compatibility contract.
2. Keep Cobra command files focused on wiring, flags, validation, and output.
3. Put reusable behavior in the owning `internal/*` capability package.
4. Preserve inspectable, versioned durable formats.
5. Keep expstore authoritative and hosted analytics downstream.
6. Test behavior at the layer that owns it.

Run focused package tests while iterating. Before Tau-internal closeout:

```bash
cd cli
go test ./...
```

Manifest, runtime, telemetry, or Kubernetes lifecycle changes also require the
smallest safe end-to-end path that proves the external contract.
