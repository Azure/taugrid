# E2E Test Contract: Notebook-to-Production Slice 1

The backbone *is* the contract: every activity must be demonstrably reachable, and slice-1 acceptance criteria are already shaped as end-to-end scenarios. This document defines what the slice-1 suite must cover, not how to implement it.

Existing coverage to build on (do not duplicate): `tests/e2e/` for cluster paths, `tools/run-plugin-e2e.mjs` / `run-button-e2e.mjs` / `run-embed-e2e.mjs` for browser paths, and `sdk/python/python/tests/` for offline units.

---

## 1. Coverage matrix

| # | Backbone activity | Slice-1 stories | E2E scenarios required |
|---|---|---|---|
| 1 | Open a ready notebook | S001, S003 | 1 offline render + 1 live-cluster dropdowns |
| 2 | Submit the notebook | S008, S010, S011, S013 | 1 render-conformance (offline) + 1 apply (live) + 1 GPU-placement (live) |
| 3 | Watch it converge | S016 | 1 offline curve render + 1 live metrics read |
| 4 | Inspect the cluster | S023 | 1 link-shape (offline) + 1 live proxy reachability |
| 5 | Recover or hand off | S027 | 1 offline diagnostics render + 1 live failure path |
| 6 | Govern tenant capacity and access | S031, S033, S034 | 1 offline authorization matrix (the core new coverage) |
| 7 | Promote and operate the run | S038 | 1 offline reproducibility record + 1 live post-teardown read |
| — | Non-backbone: test isolation | S045 | 1 browser-harness isolation scenario |
| — | Non-backbone: SDK packaging and CI | S047 | CI collection gate (no E2E scenario; the job *is* the test) |

**Gap to close first:** activity 6 has no existing coverage at all. The portal has `TestReadProfilesFiltersApplicabilityByNamespaceAndTeam` and the CLI has applicability tests, but nothing asserts the *plugin* agrees with them. Scenario E2E-A6-01 is the highest-value new test in this contract.

---

## 2. E2E-HAPPY — the demoable journey

One scenario that traverses every backbone activity in order. This is the slice-1 gate.

**Preconditions:** a cluster with a ready `TauCluster`, one ready profile applicable to the test namespace and team, a usable queue, and a Jupyter server running the SDK with the `widgets` extra.

| Step | Activity | Action | Observable |
|---|---|---|---|
| 1 | Open | Run all cells in the template notebook | Panel renders; profile and queue dropdowns populated; only ready, applicable profiles listed |
| 2 | Submit | Select the profile; click Submit | Notebook path shown; payload digest computed; no apply yet |
| 3 | Submit | Confirm (dry-run first) | Rendered RayJob is `ray.io/v1` with `suspend: true`, no `managedBy`, Kueue queue label present |
| 4 | Submit | Apply | Run handle returned; panel switches to run view; resolved namespace, queue and profile echoed |
| 5 | Watch | Wait for the loss pane | Curve drawn from house-format JSONL; source labelled as the metrics file; delta stated in words |
| 6 | Inspect | Open the Ray pane | Link present, opens in a new tab; hidden for a run with no RayCluster |
| 7 | Recover | Read the failures (if any) | Diagnostics open by default with severity, message, suggestion |
| 8 | Govern | Re-open the panel as a team the profile does not authorise | Profile absent from the dropdown; a forced submit is refused |
| 9 | Promote | Read the run record after teardown | Image, pip set, profile and payload digest retrievable; digest matches a re-submit of the same notebook |

**Pass condition:** steps 1-9 all observable in one run, with no CLI or `kubectl` typed by the researcher.

---

## 3. Per-activity scenarios

| Scenario | Activity | Maps to | What it proves |
|---|---|---|---|
| **E2E-A1-01** | Open | S001 | Panel renders with no user-authored import; missing extra fails loudly. |
| **E2E-A1-02** | Open | S003 | Dropdowns come from the cluster; a missing TauCluster disables Submit. |
| **E2E-A2-01** | Submit | S010 | Render conformance: Python plugin vs `tau run --dry-run=client`, semantically equal after normalizing generated fields. |
| **E2E-A2-02** | Submit | S013 | Apply returns a handle; a bad profile or queue produces a message that distinguishes user error from entitlement. |
| **E2E-A2-03** | Submit | S011 | **New.** A CUDA-using notebook under a GPU profile executes where the GPUs are, or is refused with a named constraint. This is the regression test for the confirmed defect. |
| **E2E-A3-01** | Watch | S016 | Curve from house-format rows; <2 points says "waiting"; a rising curve reports loss up. |
| **E2E-A4-01** | Inspect | S023 | Link shape and `rel="noopener"`; pane hidden when no RayCluster is associated. |
| **E2E-A5-01** | Recover | S027 | Diagnostics open by default; severity by text and colour; not-found names the namespace. |
| **E2E-A6-01** | Govern | S033, S034 | **Highest value.** Authorization matrix: for each (profile, namespace, team) the plugin's offered set equals the CLI's and the portal's. Includes the global-applicability case (`containsOrGlobal`). |
| **E2E-A6-02** | Govern | S031 | Documentation states single-workspace scope and the non-goal explicitly. Assertion is on doc content, not runtime. |
| **E2E-A7-01** | Promote | S038 | Repro record complete and stable across identical submissions; readable after teardown. |
| **E2E-X-01** | Non-backbone | S045 | **New.** With a user session and kernel already present on the server, run a harness and assert both survive. Reap only harness-attributed resources. |

---

## 4. Sequencing

Dependency-aware order — later scenarios assume the earlier ones pass, and the ordering follows `backlog.csv` `depends_on`:

```
E2E-A1-01  (offline, no cluster)
  └─ E2E-A1-02
       └─ E2E-A2-01  (render conformance — needs S010)
            ├─ E2E-A2-02  (apply — needs S013)
            └─ E2E-A2-03  (GPU placement — needs S011)
                 └─ E2E-A3-01   (needs a run)
                      ├─ E2E-A4-01
                      ├─ E2E-A5-01
                      └─ E2E-A7-01  (needs post-teardown read)
E2E-A6-01  (offline; independent of the run chain — run it first, it is cheapest)
E2E-X-01   (independent; requires a Jupyter server with pre-existing state)
```

**Run order recommendation:** E2E-A6-01 → E2E-A1-01 → E2E-A1-02 → E2E-A2-01 → E2E-A2-02 → E2E-A2-03 → E2E-A3-01 → E2E-A4-01 → E2E-A5-01 → E2E-A7-01 → E2E-X-01.

Rationale: the cheapest, most decisive check (authorization parity) runs first and needs no cluster. The browser-isolation scenario runs last because it needs a deliberately dirty server.

---

## 5. Explicitly out of this contract

- **Concurrent tenant isolation.** Not a slice-1 capability (decision D002, ROADMAP P10). If a reviewer wants it, it is a roadmap change, not a test gap.
- **`tfevents` ingestion.** Gated on S048; the adapter (S049) is `r2` and would add an activity-3 scenario when it lands.
- **Multi-node Indexed Job metrics.** Out of platform support today (`skills/taugrid/references/run-config.md:182`), so no scenario asserts it.
- **Serving or model-registry round trips.** Out of TauGrid's declared boundary (ROADMAP "Not planned").
