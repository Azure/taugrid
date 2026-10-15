# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

TauGrid serves platform operators responsible for GPU infrastructure and researchers running AI workloads. In the portal, operators need to understand health, capacity, queue pressure, and utilization; researchers need to understand admission, execution, and experiment activity.

## Product Purpose

TauGrid provides a unified operational layer for scheduling, running, and observing GPU workloads on Kubernetes. Success means users can quickly understand whether capacity is healthy and available, where work is waiting or running, and which surface to open for deeper investigation.

## Positioning

TauGrid connects Kubernetes GPU fleet state, Kueue admission, Ray orchestration, workload execution, experiment tracking, and cost signals in one workspace-scoped product rather than presenting each subsystem as an isolated tool.

## Operating Context

Users work across Kubernetes clusters, TauGrid workspaces, namespaces, queues, GPU node pools, jobs, Ray workloads, and experiments. Portal data may be partial when a source is unavailable, and every view must preserve workspace authorization and clearly distinguish observed state from unavailable or unknown state.

## Capabilities and Constraints

- The portal is a React and TypeScript web application backed by workspace-scoped Go APIs.
- The overview must use real portal data and link to existing Fleet, Kueue, Jobs, Experiments, and Cost surfaces.
- Platform and Workloads personas share the overview route but prioritize different operational questions.
- The new overview centers on an interactive infrastructure topology connecting sites, GPU pools, queues, and active workloads, with supporting metrics.
- Existing loading, stale-data, partial-data, authorization, and source-unavailable behavior must remain explicit.
- The portal must remain usable on narrow screens and without relying on color alone.

## Brand Commitments

Preserve the TauGrid name, direct operational voice, restrained product UI, and the existing blue accent and semantic status vocabulary. The PlanetScale interactive dashboard is a quality and interaction reference, not a visual identity to copy.

## Evidence on Hand

The repository contains real API contracts and existing portal boards for fleet inventory, GPU health and utilization, Kueue capacity, running workloads, experiment links, and cost. No customer claims, benchmark claims, or production screenshots are available for use as product evidence.

## Product Principles

- Make distributed infrastructure understandable as one connected system.
- Lead with actionable state, then provide precise drill-downs.
- Never turn missing telemetry into reassuring-looking data.
- Preserve workspace scope and authorization in every navigation path.
- Keep dense operational information scan-friendly and familiar.

## Accessibility & Inclusion

The overview must support keyboard navigation, visible focus, semantic status text, responsive layouts, and status indicators that do not depend on color alone.
