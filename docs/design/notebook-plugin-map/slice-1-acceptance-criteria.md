# Slice 1 Acceptance Criteria: TauGrid Notebook-to-Production Plugin

Given/When/Then for the 15 walking-skeleton stories. Scenario IDs match `backlog.csv` `acceptance_criteria_file` anchors and the per-activity scenarios in `e2e-test-contract.md`.

Cross-cutting ACs that apply to every story in this file:

- **No new required service.** Slice 1 adds no cluster-scoped service, proxy route, or chart dependency. Anything that would is out of slice 1 by construction (decision D005).
- **Offline-testable.** Every assertion below is reachable with injected clients/transports — no live cluster required in CI.
- **Fail loudly.** Each refusal names the offending value and the operator's next action.

---

## S001 — Template panel renders with no user import

- **Given** the platform-authored `taugrid-notebook-template.ipynb`
- **When** a researcher runs all cells without editing anything
- **Then** the TauGrid panel renders in the notebook output
- **And** the researcher typed no `import`

- **Given** `tau` is installed without the `widgets` extra
- **When** the template cell runs
- **Then** the failure names the missing extra and the install command
- **And** it does not present a partially-rendered panel

## S003 — Profile and queue dropdowns come from the TauCluster

- **Given** a readable `TauCluster/cluster` with at least one workload profile
- **When** the panel renders
- **Then** profile and queue dropdowns are populated from the cluster
- **And** the queue defaults to `spec.workspaceDefaults.defaultQueue` when present

- **Given** no readable `TauCluster`
- **When** the panel renders
- **Then** it states that no TauCluster was found and names the resource it looked for
- **And** Submit is disabled rather than submitting an unresolvable target

## S008 — Resolve the current notebook and package it with a runner

- **Given** a notebook open in a Jupyter session
- **When** the researcher clicks Submit
- **Then** the resolved notebook path is shown before the payload is built
- **And** the staged payload contains the notebook bytes plus the runner

- **Given** the notebook cannot be resolved from the session
- **When** Submit is clicked
- **Then** the notebook field is empty, editable, and the panel shows a "notebook path not resolved" state
- **And** submission does not proceed

## S010 — Render a ray.io/v1 RayJob with the Kueue contract intact

- **Given** a resolved profile and queue
- **When** the panel renders the workload
- **Then** the result is `apiVersion: ray.io/v1`, `kind: RayJob`
- **And** `spec.suspend` is `true` and `spec.managedBy` is left unset
- **And** the Kueue queue label is present

- **Given** the same logical job
- **When** rendered by the Python plugin and by `tau run --dry-run=client`
- **Then** the two RayJob objects are semantically equal after normalizing generated fields

## S011 — Work that needs a GPU executes where the GPUs are

- **Given** a profile with `gpusPerWorker > 0` and a notebook that calls CUDA in its own kernel
- **When** the researcher submits
- **Then** the notebook's own kernel executes on compute that has the requested GPUs
- **And** the panel does not label the run GPU-backed on the strength of worker GPUs alone

- **Given** a notebook that dispatches its GPU work through Ray instead
- **When** the researcher submits against the same profile
- **Then** the run is accepted and the panel states that GPU work executes on workers

- **Given** a shape the platform cannot execute correctly
- **When** the researcher submits
- **Then** submission is refused with a reason naming the constraint

## S013 — Apply the workload, with actionable errors

- **Given** a rendered RayJob and a reachable cluster
- **When** the panel applies it
- **Then** a run handle is returned and the panel switches to the run view

- **Given** a profile that does not exist, or a queue the workspace cannot use
- **When** the panel applies it
- **Then** the error names the offending value
- **And** the message distinguishes "fix your config" from "ask the platform owner"

## S016 — Loss curve from house-format JSONL metrics

- **Given** a run emitting house-format rows (`_step`, `_timestamp`, `train/loss`) into the published history glob
- **When** the loss pane renders
- **Then** it draws the loss-vs-step curve from those rows
- **And** it labels the source as the metrics file
- **And** it states the delta in words and numbers

- **Given** fewer than two points
- **When** the pane renders
- **Then** it says it is waiting for the first steps rather than drawing a single dot

- **Given** the curve is rising
- **When** the pane renders
- **Then** it says so in the warn colour rather than presenting a neutral line

## S023 — Link to the Ray dashboard

- **Given** a run whose RayCluster head Service is known
- **When** the Ray pane renders
- **Then** it renders a hyperlink to the portal proxy path for that cluster
- **And** the link opens in a new tab with `rel="noopener"`

- **Given** a run with no associated RayCluster
- **When** the panel renders
- **Then** the Ray pane is hidden rather than showing a dead link

## S027 — Diagnostics with severity, message and suggestion

- **Given** a failed run with diagnostics present
- **When** the panel renders
- **Then** the diagnostics pane is open by default
- **And** each entry shows severity, message and suggestion
- **And** severity is conveyed by text as well as colour

- **Given** a run that is not found in the namespace
- **When** the panel renders
- **Then** it says the run was not found and names the namespace searched

## S031 — Scope documented as single-workspace, isolation a non-goal

- **Given** the plugin's user-facing documentation
- **When** a reader looks for tenant-isolation guarantees
- **Then** the documentation states that one workspace is active per cluster
- **And** it states that the path check is a containment check, not a filesystem sandbox and not tenant isolation
- **And** it names what a shared-server operator must provide instead

- **Given** any story that implies isolation
- **When** it is reviewed
- **Then** it is either re-scoped to correctness or deferred to the roadmap's multi-tenancy item

## S033 — Only ready profiles, from the resolved status catalog

- **Given** a TauCluster whose profile readiness is published under `status.workloadProfiles.profiles`
- **When** the panel lists profiles
- **Then** only profiles the current generation reports ready are offered
- **And** readiness is read from the status catalog, not from a field inside the spec entry

- **Given** a profile that is declared in spec but missing or unready in status
- **When** the panel lists profiles
- **Then** it is not offered
- **And** the panel does not present it as merely "unavailable" while still allowing selection

## S034 — Profiles filtered by namespace and team applicability

- **Given** a profile whose `applicability.namespaces` and `applicability.teams` do not include the selected namespace or team
- **When** the panel lists profiles
- **Then** that profile is not offered

- **Given** a profile with empty applicability lists (global)
- **When** the panel lists profiles
- **Then** it is offered, matching `containsOrGlobal` semantics used elsewhere in the platform

- **Given** a selection that bypassed the dropdown (for example a stale widget state)
- **When** the panel submits
- **Then** submission is refused because the profile does not apply to the namespace and team

## S038 — Run carries a reproducibility record

- **Given** a submitted run
- **When** its record is read
- **Then** it identifies the runtime image, the pip set, the workload profile, and the payload digest
- **And** the record is retrievable after the RayCluster is torn down

- **Given** two runs submitted from the same notebook and profile
- **When** their records are compared
- **Then** the payload digests match, so equality is meaningful

## S045 — Browser harnesses clean up only what they created

- **Given** a Jupyter server that already holds a user's session and kernel
- **When** a browser e2e harness runs
- **Then** that session and kernel still exist afterwards
- **And** no workspace other than the harness's own is cleared

- **Given** a prior crashed harness run left an orphaned session
- **When** the harness starts
- **Then** it reaps only sessions and kernels it can attribute to its own run identity

## S047 — Widgets extra declares its imports; CI installs it

- **Given** a clean environment
- **When** `pip install -e '.[dev,widgets]'` runs
- **Then** `tau.jupyter.server` and `tau.jupyter.metrics` import successfully
- **And** the SDK test job collects the jupyter tests rather than erroring

- **Given** the SDK test job as configured in CI
- **When** it runs
- **Then** every module under test is importable from the declared extras alone
- **And** no test module is silently skipped for a missing dependency

---

## INVEST check

| Story | Independent | Negotiable | Valuable | Estimable | Small | Testable | Note |
|---|---|---|---|---|---|---|---|
| S001 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S003 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S008 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S010 | ✓ | ✓ | ✓ | ✓ | ✗ | ✓ | **Largest item (size 10). Split if the renderer port exceeds one increment.** |
| S011 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | Depends on S010. |
| S013 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | Dependency hub (7 downstream). |
| S016 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S023 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S027 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S031 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S033 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S034 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S038 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S045 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | — |
| S047 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | Already implemented in `7c07fe4`. |
