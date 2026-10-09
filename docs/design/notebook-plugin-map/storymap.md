# Story Map: TauGrid Notebook-to-Production Plugin

> **Outcome:** A researcher submits the notebook they are already working in, watches it train on the GPUs it actually needs, and the resulting run is reproducible, promotable and auditable — inside TauGrid's declared boundary as a workflow and lifecycle layer.
> **Personas:** researcher, platform-engineer, tenant-admin, mlops-engineer, sre, reviewer
> **Slicing strategy:** walking-skeleton / mvp / r2

## Activity: Open a ready notebook

### Task: Get the panel

- [slice:walking-skeleton] [persona:platform-engineer] As a platform engineer, I want to ship a notebook template whose panel renders with no import the researcher types, so that the end user only ever clicks
- [slice:mvp] [persona:researcher] As a researcher, I want to paste a one-line cell into an existing notebook, so that I can add the panel without starting over from the template

### Task: Choose what to run

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want profile and queue dropdowns populated from the TauCluster, so that I pick platform policy instead of typing it
- [slice:mvp] [persona:researcher] As a researcher, I want to see which profile and queue were resolved and why, so that I trust the defaults rather than guessing
- [slice:mvp] [persona:researcher] As a researcher, I want each offered profile to say whether a notebook running in its own kernel gets a GPU, so that I am not surprised by a silent CPU fallback

### Task: Pick the files to ship

- [slice:mvp] [persona:researcher] As a researcher, I want to pick companion files sitting beside the notebook, so that the run carries what it imports
- [slice:mvp] [persona:researcher] As a researcher, I want the picker to bound what it reads and refuse an oversized file with a reason, so that a large file cannot exhaust the notebook server

## Activity: Submit the notebook

### Task: Package the notebook

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want the panel to resolve the notebook I am working in and package it with a runner, so that the notebook itself is the job
- [slice:mvp] [persona:researcher] As a researcher, I want unsupported notebook shapes refused with a reason, so that I never get a silently wrong job

### Task: Render the workload

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want the panel to render a ray.io/v1 RayJob with Kueue's admission contract intact, so that submitting still enters fair-share queueing
- [slice:walking-skeleton] [persona:researcher] As a researcher, I want the work that needs a GPU to execute where the GPUs are, so that a notebook doing its own CUDA training does not silently fall back to CPU
- [slice:mvp] [persona:platform-engineer] As a platform engineer, I want the Python renderer to match the Go renderer for the same logical job, so that the plugin cannot drift from the CLI

### Task: Apply it

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want the panel to apply the workload and give a clear actionable error when the profile or queue is wrong, so that I know whether to fix my config or ask the platform owner
- [slice:mvp] [persona:researcher] As a researcher, I want a dry-run that renders without applying, so that I can inspect the manifest before it consumes quota
- [slice:mvp] [persona:researcher] As a researcher, I want the resolved namespace, queue and profile echoed back after submit, so that I can confirm the platform applied the policy I expected

## Activity: Watch it converge

### Task: Read training progress

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want a loss curve read from the run's house-format JSONL metrics, so that I can see convergence without leaving the notebook and without a second metrics pipeline
- [slice:mvp] [persona:researcher] As a researcher, I want loss parsed from the job's stdout when no metrics file exists, so that a plain print statement is enough
- [slice:r2] [persona:researcher] As a researcher, I want the portal series API used as a fallback source, so that history remains readable after the run ends

### Task: Read GPU usage

- [slice:mvp] [persona:researcher] As a researcher, I want per-device GPU utilization with explicit observed flags, so that a real zero is distinguishable from a value that was never reported
- [slice:mvp] [persona:researcher] I want scrape coverage and framebuffer usage shown together, so that I can tell a partial scrape from a healthy one

### Task: Keep it live

- [slice:mvp] [persona:researcher] As a researcher, I want automatic polling with a visible interval and a stop control, so that the panel's updates are explainable
- [slice:mvp] [persona:researcher] As a researcher, I want a completed run to say that live GPU samples are gone, so that I am never shown a false zero

## Activity: Inspect the cluster

### Task: Open Ray

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want a link to the Ray dashboard for my run's cluster, so that I can drill into Ray without knowing the proxy URL scheme
- [slice:mvp] [persona:researcher] As a researcher, I want the dashboard embedded only when it can actually load, so that I never stare at a blank frame with no error

### Task: Check admission and pods

- [slice:mvp] [persona:researcher] As a researcher, I want the admission state and the Kueue message verbatim, so that I can tell waiting-for-quota apart from broken
- [slice:mvp] [persona:researcher] As a researcher, I want the pod list with phase, node and restart count, so that I can spot a crash loop myself

## Activity: Recover or hand off

### Task: Read the failure

- [slice:walking-skeleton] [persona:researcher] As a researcher, I want diagnostics with severity, message and suggestion, so that a failure tells me what to do next
- [slice:mvp] [persona:researcher] As a researcher, I want the suggested next command rendered copy-pasteable, so that recovery does not require guessing

### Task: Share the evidence

- [slice:mvp] [persona:reviewer] As a reviewer, I want a standalone HTML export of the panel, so that I can attach the run's evidence to a report or a pull request
- [slice:r2] [persona:reviewer] As a reviewer, I want a deep link into the portal experiment page, so that I can see the full run rather than a summary

## Activity: Govern tenant capacity and access

### Task: State what is governed

- [slice:walking-skeleton] [persona:platform-engineer] As a platform engineer, I want the plugin's scope documented as single-active-workspace with isolation explicitly a non-goal, so that nobody mistakes a path check for tenant isolation
- [slice:mvp] [persona:tenant-admin] As a tenant admin, I want to see which workspace, namespace and team a profile authorizes, so that I know who is entitled to it

### Task: Keep selection correct

- [slice:walking-skeleton] [persona:tenant-admin] As a tenant admin, I want only ready profiles offered from the cluster's resolved status catalog, so that a stale or unready profile cannot be selected
- [slice:walking-skeleton] [persona:tenant-admin] As a tenant admin, I want profiles filtered by the namespace and team they authorize, so that the plugin cannot offer capacity a tenant is not entitled to
- [slice:mvp] [persona:platform-engineer] As a platform engineer, I want the plugin's readiness and applicability rules to match the CLI and the portal, so that there is one authorization policy rather than three
- [slice:mvp] [persona:tenant-admin] As a tenant admin, I want a submission refused when the profile does not apply, so that entitlement is enforced at the boundary rather than in a dropdown

### Task: Keep it auditable

- [slice:r2] [persona:platform-engineer] As a platform engineer, I want an auditable record of workspace and profile changes, so that access changes can be reviewed after the fact

## Activity: Promote and operate the run

### Task: Reproduce it

- [slice:walking-skeleton] [persona:mlops-engineer] As an MLOps engineer, I want the submitted run to carry a reproducibility record, so that I can rebuild the environment that produced a result
- [slice:mvp] [persona:mlops-engineer] As an MLOps engineer, I want the payload digest visible on the workload, so that I can confirm which notebook bytes actually ran

### Task: Promote the artifacts

- [slice:mvp] [persona:mlops-engineer] As an MLOps engineer, I want the checkpoints and artifacts a run produced to be discoverable, so that a result can leave the notebook
- [slice:mvp] [persona:mlops-engineer] As an MLOps engineer, I want to promote a checkpoint to a durable location, so that it survives the RayCluster being torn down

### Task: Operate it

- [slice:mvp] [persona:sre] As an SRE, I want run lifecycle state visible after the RayCluster is torn down, so that I can tell what happened to a finished run
- [slice:mvp] [persona:sre] I want the run's metrics history retained beyond the run, so that I can compare it against later runs
- [slice:r2] [persona:sre] As an SRE, I want per-team cost attribution for the run, so that consumption can be charged to the team that caused it

## Non-backbone / cross-cutting

### Theme: Test isolation of the browser harnesses

- [slice:walking-skeleton] [persona:platform-engineer] As a platform engineer, I want the browser e2e harnesses to clean up only the sessions and kernels they created, so that running a test never destroys a user's in-memory work
- [slice:mvp] [persona:platform-engineer] As a platform engineer, I want each harness to use a unique run identity and its own workspace, so that concurrent test invocations do not collide

### Theme: SDK packaging and CI

- [slice:walking-skeleton] [persona:platform-engineer] As a platform engineer, I want the widgets extra to declare every module it imports and CI to install it, so that installing the extra is sufficient and the jupyter tests actually run

### Theme: Metrics format convergence

- [slice:mvp] [persona:platform-engineer] As a platform engineer, I want a written comparison of the tfevents format against the house W&B-style JSONL format, so that we decide on evidence which formats the metrics collector should accept instead of adding a service per tracker
- [slice:r2] [persona:platform-engineer] As a platform engineer, I want tfevents converted into house-format rows rather than served by a parallel dashboard, so that one metrics pipeline keeps serving every framework

<!--
Author notes:
- Activities 1-5 are the researcher backbone carried forward from
  docs/design/notebook-plugin.md. Activities 6-7 are the platform/ops columns
  added for this map (decision D001 in design.md).
- Every activity has at least one [slice:walking-skeleton] story, so slice 1 is
  demoable end to end. Slice 1 is capped at 15 stories.
- The Govern column encodes decision D002: single active workspace, concurrent
  tenant isolation is an explicit non-goal. S033/S034 are therefore correctness
  stories (one policy everywhere), not tenancy-feature stories.
- The Promote and operate column encodes decision D003: platform-owned lifecycle
  only. Serving, model registry and framework internals stay out per ROADMAP.md.
- The Metrics format convergence theme encodes D004/D005: the maintainer asked
  for a format comparison (PR #285) and wants the existing metrics collector
  extended, so the TensorBoard service is withdrawn pending that analysis.
  See contradiction C1 in design.md.
-->
