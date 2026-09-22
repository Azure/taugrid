---
version: 1
slug: "portal-frontend-src-overview-tsx"
primary_target: "portal/frontend/src/Overview.tsx"
related_targets: ["portal/frontend/src/styles.css","portal/frontend/src/Boards.tsx"]
---

## Scope and mode

TauGrid portal overview at `/portal`, Operate mode, shared by Platform and Workloads personas.

## Audience, job, and task

Platform operators and researchers need to understand how GPU capacity, scheduler admission, and active workloads relate, then drill into the board that explains a bottleneck or failure.

## Content and constraints

Use real workspace-scoped fleet, cluster telemetry, queue, workload, experiment, and cost data. Preserve explicit loading, stale, partial, unavailable, and unknown states. Preserve the incumbent TauGrid visual system and existing navigation.

## Direction

Infrastructure atlas: a topology-first canvas groups GPU inventory by site and pool, shows allocated-versus-available capacity directly inside each node, and connects capacity through Kueue pressure to admitted workloads. The memorable moment is selecting a site and seeing its concrete GPU pools while the rest of the operational path remains in view.

## Unresolved decisions

Future API work may add explicit workload-to-node placement edges and historical trend series. The current surface must not infer either.
