# Backlog: TauGrid Notebook-to-Production Plugin

Method: **WSJF** (SAFe default). Score = (business value + time criticality + risk reduction/opportunity) / job size. Full scoring in `backlog.csv`; story text in `storymap.md`.

- **Total stories:** 49
- **Slice 1 (walking-skeleton):** 15 — at the cap, covers all 7 activities
- **Slice 2 (mvp):** 22
- **Slice 3 (r2):** 9
- **Non-backbone:** 5 across three themes

Slice-1 dependency feasibility: **PASS** — every hard dependency of a slice-1 story is also in slice 1. No cycles.

---

## Head of the queue (top 12)

| # | ID | Slice | WSJF | Size | Story | Why it ranks here |
|---|---|---|---|---|---|---|
| 1 | S031 | 1 | 7.33 | 3 | Document scope as single-active-workspace; isolation a non-goal | Cheapest high-value item on the board. The design *already* disclaims tenant isolation; the gap is that nothing forces a reader to notice. Prevents a whole class of misreading. |
| 2 | S047 | 1 | 6.67 | 3 | Widgets extra declares its imports; CI installs it | Already implemented in `7c07fe4`. Without it the jupyter tests cannot even collect, so it unblocks all other verification. |
| 3 | S045 | 1 | 5.75 | 4 | Browser harnesses clean up only what they created | Confirmed defect: current cleanup deletes every session and kernel and clears the shared workspace. Destroys real user work. |
| 4 | S039 | 2 | 5.00 | 3 | Payload digest visible on the workload | Small and directly serves the MLOps half — "which notebook bytes ran?" |
| 5 | S014 | 2 | 4.67 | 3 | Dry-run renders without applying | Cheap guard against consuming quota on a bad config. |
| 6 | S001 | 1 | 4.50 | 4 | Template panel renders with no user import | The mechanism the entire design depends on. |
| 7 | S033 | 1 | 4.40 | 5 | Only ready profiles, from the resolved status catalog | Confirmed defect: reads `spec.workloadProfiles[].status.state`, a field the CRD does not populate, so unready profiles are selectable. |
| 8 | S015 | 2 | 4.33 | 3 | Echo resolved namespace/queue/profile | Cheap confidence that policy applied as expected. |
| 9 | S023 | 1 | 4.25 | 4 | Link to the Ray dashboard | Drill-down without knowing the proxy scheme. |
| 10 | S025 | 2 | 4.25 | 4 | Admission state and Kueue message verbatim | The only useful signal while a run is queued. |
| 11 | S034 | 1 | 4.17 | 6 | Filter profiles by namespace and team applicability | Authorization. The CLI and portal already enforce this; the plugin does not, so one platform gives two answers. |
| 12 | S038 | 1 | 4.00 | 5 | Run carries a reproducibility record | Production MLOps entry point — without it a result is not defensible. |

## Slice 1 — the demoable journey (15)

Slice 1 is the smallest set that lets someone demo the whole story: open → submit → train → inspect → recover → govern → reproduce.

| Activity | Slice-1 stories |
|---|---|
| Open a ready notebook | S001, S003 |
| Submit the notebook | S008, S010, S011, S013 |
| Watch it converge | S016 |
| Inspect the cluster | S023 |
| Recover or hand off | S027 |
| Govern tenant capacity and access | S031, S033, S034 |
| Promote and operate the run | S038 |
| Non-backbone: test isolation | S045 |
| Non-backbone: SDK packaging and CI | S047 |

**Three of the fifteen are defect fixes confirmed on PR #300** (S011 GPU placement, S033 readiness, S034 applicability), and **one is already implemented** (S047). The remaining eleven are the minimum coherent journey.

## What is deliberately not in slice 1

- **Loss from stdout** (S017) and **portal series fallback** (S018) — Slice 1 proves the house-format metrics file path first; the second channel is an enhancement.
- **Live polling** (S021) — Slice 1 renders correct state on demand; continuous refresh is Slice 2.
- **Policy parity test** (S035) — S034 fixes the plugin; S035 makes the three implementations provably agree. Valuable, but it depends on S034 and is a guard rather than a capability.
- **tfevents adapter** (S049) — intentionally `r2`, gated on the format analysis (S048) the maintainer asked for.

## MoSCoW rollup

| Class | Count | Notes |
|---|---|---|
| Must | 15 | Exactly slice 1 — deliberate: Must == the demoable journey |
| Should | 25 | Slice 2 breadth: richer states, parity, promotion, retention |
| Could | 9 | Slice 3: fallbacks, deep links, audit, cost, adapter |

## Scoping notes for the team

- **S012** (Python/Go renderer parity) carries the largest hidden risk: the design doc calls renderer drift the top introduced risk, and the conformance suite is the mitigation. It is sized 5 but may grow.
- **S010** is the single largest item (size 10) and sits on the critical path — S011, S013, and everything downstream depend on it. Split it if the port proves larger than one increment.
- **S013** is another dependency hub: seven stories hard-depend on it.
- **S048/S049** are the only stories that touch the TensorBoard question, and both are gated on maintainer review of the format analysis. Do not start S049 before S048 is accepted.
