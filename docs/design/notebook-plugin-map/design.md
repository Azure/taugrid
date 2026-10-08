# Design: TauGrid Notebook-to-Production Plugin (Story Map)

Status: **draft for review.** Companion to `storymap.md`, `backlog.md`, `slice-1-acceptance-criteria.md`, `e2e-test-contract.md`.

This map extends the committed design at [`../notebook-plugin.md`](../notebook-plugin.md) — read that first; it holds the transport, payload, and renderer contracts. This document adds the personas, evidence, and decisions needed to plan delivery across four dimensions the requester named: **multi-tenant**, **model training**, **Jupyter**, and **production MLOps**.

---

## 1. The question this work answers

> Can a researcher who is already working in a notebook get that notebook onto the right GPUs, watch it train, and come away with a run that is reproducible, promotable and auditable — without the platform silently misrepresenting either its isolation guarantees or where the GPUs actually are?

Two halves, deliberately joined:

- **The researcher half** (activities 1-5) — carried forward from `notebook-plugin.md`.
- **The platform half** (activities 6-7) — governance and lifecycle, added because the current design is thin exactly where reviewers pushed back.

The join is the point. Three of the five open review comments on PR #300 are about the seam between the two halves, not about either half alone.

---

## 2. Context loop trace

Mining order and what each source settled.

| # | Source | Signal |
|---|---|---|
| 1 | `glob docs/design/*` | Only `notebook-plugin.md` remains. The storymap/backlog/AC files from a prior session were **untracked scratch** — `git log --all` shows they never entered git. No prior map to extend. |
| 2 | `**/.user-story-mapping/**` | No `state.json`. No prior-run memory. |
| 3 | `tau/jupyter/`, `tau/widgets/` | Implemented surface is real and wide: server extension, submit, runs, metrics, destinations, notebook_files; panel, embed, status, watcher, kube, session. |
| 4 | `ROADMAP.md:17-21` | **"Today a cluster runs one active workspace, and researcher isolation is still gated on negative-access tests."** Multi-tenancy is a *Next* item, not delivered. |
| 5 | `site/.../workspaces.md:38` | Concurrent tenant isolation is *"out of scope for this stage."* |
| 6 | `docs/design/notebook-plugin.md:208-212` | The design already disclaims isolation: the path check is *"a directory-containment check, not a complete filesystem sandbox"* and *"Shared-server operators must not treat either check as tenant isolation."* |
| 7 | `core/resourceprofile/workload.go:416` | `ProfileApplicability{Namespaces, Teams, Lanes}` exists with a `containsOrGlobal` check. |
| 8 | `portal/internal/portal/jobs/jobs.go:253` | The portal's Jobs board **already filters profiles by namespace and team applicability** (`TestReadProfilesFiltersApplicabilityByNamespaceAndTeam`). |
| 9 | `sdk/python/python/tau/_profile.py:48-90` | The Python plugin reads **neither** readiness-from-status **nor** applicability. It filters on `spec.workloadProfiles[].status.state`, a field the CRD does not populate. |
| 10 | `controllers/tau-core/api/v1alpha1/types.go:188` | Readiness actually lives at `status.workloadProfiles.profiles` (`profile.ProfileSetStatus`). |
| 11 | `sdk/python/python/tau/_render.py:199-207` | Head pod is pinned `num-gpus: "0"` / `num-cpus: "0"`; GPUs exist only on `workerGroupSpecs`. The notebook executes on the **head**. |
| 12 | `ROADMAP.md:73-83` | **"Not planned": model code, data-preparation logic, serving application semantics.** TauGrid is "a workflow and lifecycle layer." |
| 13 | `GOVERNANCE.md:58-66` | Changes "with broad or difficult-to-reverse impact" — including **"new required services or major dependencies"** and **"security or identity models"** — must begin with an issue or design proposal. |
| 14 | `GOVERNANCE.md:79-85` + `.github/CODEOWNERS:1` | All paths owned by `@Azure/taugrid-maintainers`. One code-owner approval required; authors cannot self-approve. |
| 15 | `portal/README.md:52-54` | **"The legacy `taugrid-portal experiment offload metrics` command remains only as an offline/recovery compatibility tool. New workload sidecars use the standalone `taugrid-metrics-collector`."** |
| 16 | `cli/SDK_GUIDE.md:201`, `portal/README.md:60` | Online metric rows **must** carry numeric W&B-style `_step` and `_timestamp`. |
| 17 | `skills/taugrid/references/run-config.md:182` | The metrics sidecar *"supports single-pod direct Jobs and RayJob heads, **not multi-node Indexed Jobs**."* |
| 18 | PR #300 review comments (5) | See §3. |
| 19 | PR #285 review comments (4) | See §3. |

**Hypothesis: STABLE.** This is a fresh map for the notebook-plugin work, widened to platform concerns, anchored on the committed design doc's decisions and the maintainers' stated preferences.

---

## 3. Maintainer preference — the answer to "figure out the preference"

This is the most valuable output of the context loop. Preference is **not uniform** and one earlier decision now conflicts with it.

### 3.1 Stated preference table

| ID | Preference | Evidence | Confidence |
|---|---|---|---|
| **P1** | The interactive notebook itself is wanted. | `[comment: chokevin PR#285, 2026-09-18]` *"Overally I'm totally onboard with an interactive notebook."* | High |
| **P2** | TensorBoard is **not** the preferred metrics surface. | `[comment: chokevin PR#285]` *"orthogonal to how we want to display per job metrics… not the same format as offline W&B metrics which is what the industry seems to use today."* | High |
| **P3** | Extend the **existing** metrics sidecar to handle more log formats; do not add a parallel one. | `[comment: chokevin PR#285]` *"We do already have a metrics offload sidecar we should configure that container to handle the different metrics logging but again we should make sure it works as broadly as possible."* | High |
| **P4** | W&B-style JSONL (`_step`, `_timestamp`; `train/loss`) is the house format. | `[code: cli/SDK_GUIDE.md:201]`, `[code: portal/README.md:60]`, `[code: sdk/python/python/README.md:348]` *"W&B-shaped"* | High |
| **P5** | New sidecars use **`taugrid-metrics-collector`**; the portal offload verb is legacy/recovery only. | `[code: portal/README.md:52-54]` | High |
| **P6** | A format analysis is required **before** committing to a tracker integration. | `[comment: chokevin PR#285]` *"I would need some analysis between the tensorboard format and the existing format… if they are similar this should be a simple integration but if they are too different we should try to coincide the logging as much as possible."* | High |
| **P7** | Local-first is a constraint: no remote control plane required. | `[code: cli/SDK_GUIDE.md:137]` *"Local recovery should work without W&B SaaS, ADX, or any remote control plane."* | High |
| **P8** | A local TauGrid path with no ADX backend is worth considering. | `[comment: chokevin PR#285]` *"or maybe even local taugrid without a backing adx cluster could be something to consider."* | Medium |
| **P9** | TauGrid owns workflow and lifecycle only. | `[code: ROADMAP.md:73-83]` | High |
| **P10** | Multi-tenant is *Next*, not delivered; today one active workspace. | `[code: ROADMAP.md:19-21]` | High |
| **P11** | New required services / major deps need a design proposal and maintainer consensus. | `[code: GOVERNANCE.md:58-66]` | High |
| **P12** | One maintainer code-owner approval; authors cannot self-approve. | `[code: GOVERNANCE.md:79-85]`, `[code: .github/CODEOWNERS:1]` | High |
| **P13** | Metrics sidecar does not cover multi-node Indexed Jobs today. | `[code: skills/taugrid/references/run-config.md:182]` | High |
| **P14** | Authorization must be one policy: applicability is enforced in the CLI and portal already. | `[code: core/resourceprofile/workload.go:416]`, `[code: portal/internal/portal/jobs/jobs.go:253]` | High |
| **P15** | The notebook kernel has no GPU; GPU capacity is worker-only. | `[code: tau/_render.py:199]`, `[comment: feiskyer PR#300, 2026-10-08]` | High |
| **P16** | Browser-harness cleanup must be ownership-scoped and uniquely identified. | `[comment: feiskyer PR#300, 2026-10-08]` | High |

### 3.2 The two reviewers are asking different things

`chokevin` (maintainer, direction) and `feiskyer` (reviewer, correctness) are not in conflict, but they are operating at different levels:

- **chokevin** is asking *"is this the right shape for TauGrid?"* — and answers it as: keep the notebook, but converge metrics on the house W&B format through the existing collector, and stop treating TensorBoard as a first-class surface.
- **feiskyer** is asking *"is this implementation correct?"* — readiness source, applicability enforcement, GPU placement, cleanup ownership. All four are implementation defects against preferences already established elsewhere in the repo.

That split matters for sequencing: feiskyer's items are **slice 1** (defects, cheap, unblock the PR). chokevin's items are **a decision the map must record** (format convergence), not defects.

### 3.3 Contradictions flagged

**C1 — CONFLICT: the TensorBoard sidecar decision contradicts P2/P3/P11.**

The design proposal discussed earlier in this engagement selected *"run TensorBoard as a service, proxied by the portal."* That directly conflicts with:

- **P2** — the maintainer says TensorBoard is orthogonal to how per-job metrics should be displayed;
- **P3** — the maintainer wants the *existing* collector extended, not a parallel path;
- **P11** — a new required service (TensorBoard + proxy route + sidecar) is exactly the class of change GOVERNANCE sends to a design proposal.

**Recommendation:** treat the TensorBoard service as **withdrawn pending P6**. The defensible path is: emit/consume house-format JSONL through `taugrid-metrics-collector`, and treat `tfevents` as an *adapter* to that format — a reader, not a service. Story **S018** encodes this and is deliberately `r2`, gated on the format analysis.

**C2 — the plugin presents profiles the platform would not authorize.**

`portal/internal/portal/jobs/jobs.go` filters by applicability; `_profile.py` does not. Same platform, two answers to "may this team use this profile?" This is a correctness gap against **P14**, and it is *not* a multi-tenancy feature request — it is one policy being implemented three ways (CLI, portal, plugin).

**C3 — "select a GPU profile" does not mean "this notebook gets a GPU".**

`_render.py` puts the notebook on a CPU-only head while the profile's GPUs attach to workers. A researcher picking `training-8gpu` for a notebook that calls `.cuda()` gets a CPU fallback or a hard failure — the UI implies otherwise. Against **P15**.

**C4 — `docs/tau/tau-metrics-offload-sidecar.md` is referenced but absent.** `images/tau/README.md:19` links it; `glob docs/tau/*metrics*` returns nothing. Dead documentation pointer, worth fixing while in the area.

---

## 4. Personas

| Persona | Goal in this surface | Will not do |
|---|---|---|
| **Researcher** (primary) | Click Submit, watch loss + GPU, get a result out | Read YAML, learn Kueue, write imports, reason about which node has the GPU |
| **Platform engineer** (secondary) | Ship a template, own profiles/queues/RBAC, keep one authorization policy | Rebuild a dashboard per incident |
| **Tenant admin** (new) | Know who is entitled to which capacity; keep entitlement enforced | Hand-edit ClusterQueue per request |
| **MLOps engineer** (new) | Reproduce a run, promote its artifacts, trace lineage | Own model code or serving semantics |
| **SRE** (new) | See lifecycle after teardown; attribute cost; audit access changes | Debug a researcher's loss curve |
| **Reviewer / collaborator** (tertiary) | Read evidence that a run trained and converged | Run anything |

---

## 5. Backbone criteria (recorded for reproducibility)

| Criterion | Choice | Rationale |
|---|---|---|
| Frame | **Activity flow** | Patton classic; matches the narrative the requester asked for. |
| Persona perspective | **Multiple parallel** | The requester named multi-tenant + MLOps; a single researcher perspective cannot hold governance. |
| Time horizon | **Single run session** (activities 1-5) + **governance/promotion cycle** (activities 6-7) | Documented deviation: the 5 researcher activities are one session; governance is inherently longer. Mixing is deliberate, not accidental. |
| Granularity | **7 activities** | Within the 5-7 default plus one; hard max is 10. |
| Scope | **Happy path + error recovery** | The plugin already models queued/failed/complete states, so recovery is in-scope, not deferred. |
| Aggregation | **Single role per activity** | Keeps columns readable; handoffs are not the activity. |

**Backbone (read aloud — this should sound like a story):**

1. Open a ready notebook
2. Submit the notebook
3. Watch it converge
4. Inspect the cluster
5. Recover or hand off
6. Govern tenant capacity and access
7. Promote and operate the run

---

## 6. Decisions log

| ID | Decision | Consequence | Source |
|---|---|---|---|
| **D001** | Backbone extends to **7 activities** (5 researcher + 2 platform/ops). | Platform journeys get first-class columns; story count rises to 47. | `[user-stated]` |
| **D002** | Multi-tenant target is **current reality**: single active workspace, isolation an **explicit non-goal**. | Activity 6 becomes *correctness* (one policy) plus *documentation*, not a tenancy feature. Aligns with ROADMAP P10. | `[user-stated]`, `[code: ROADMAP.md:19]` |
| **D003** | Production MLOps scope is the **platform-owned slice only**: reproducibility, lineage, promotion, lifecycle, audit, cost. | Serving/model-registry/framework work stays out per P9. | `[user-stated]`, `[code: ROADMAP.md:73]` |
| **D004** | Metrics converge on the **house W&B-style JSONL format** via `taugrid-metrics-collector`. | The loss pane reads house format, not `tfevents`. Encodes P4/P5. | `[code: portal/README.md:52]`, `[comment: chokevin]` |
| **D005** | The **TensorBoard service is withdrawn** pending the P6 format analysis; `tfevents` becomes an adapter, not a service. | Removes a new required service and its proxy route; resolves C1. | `[comment: chokevin]`, `[code: GOVERNANCE.md:58]` |
| **D006** | Slice 1 is capped at **15 stories** and covers all 7 activities. | Slice 1 is the demoable journey: submit → train → authorise → reproduce. | `[skill: user-story-mapping]` |

---

## 7. Stage-local gaps to resolve

| Gap | Stage | Resolution |
|---|---|---|
| WSJF sizes for S001-S047 | Step 4 | Sized in `backlog.csv` from file-level scope; revisit with the team. |
| Exact `taugrid-metrics-collector` interface for non-JSONL inputs | Step 4a | Deferred — see Q-02. |
| Whether the runner executes the notebook on GPU-bearing compute vs restricting to Ray-dispatching notebooks | Step 4a | Both options encoded; **S011** is the decision point. feiskyer offered both. |

## 8. Open questions (deferrable)

- **Q-01** — Does `taugrid-metrics-collector` gain `tfevents` parsing, or does a separate reader convert to JSONL first? Affects effort for S018 only. `[inferred — see C1]`
- **Q-02** — Is the local-TauGrid-without-ADX path (P8) in scope for this map, or a separate one? Currently assumed out of scope; activity 7's retention stories assume a durable store exists. `[inferred]`
- **Q-03** — Which persona owns profile applicability *policy* (who declares namespaces/teams on a profile)? Assumed `platform-engineer` writes, `tenant-admin` consumes. `[inferred]`

---

## 9. What the map deliberately excludes

Per **D003** and P9, the following are out of scope and are *not* gaps: model registry, serving/inference, framework internals, data-preparation logic, and concurrent tenant isolation (P10). If a reviewer wants any of these, the correct move is a roadmap change under GOVERNANCE, not a story in this map.
