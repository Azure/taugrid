# Handoff: TauGrid Notebook-to-Production Plugin

## What was produced

| File | Contents |
|---|---|
| `design.md` | Personas, context-loop trace, **the maintainer-preference table (P1-P16)**, contradictions, backbone criteria, decisions log D001-D006, gaps |
| `storymap.md` | 7-activity backbone, 49 stories, 3 cross-cutting themes |
| `storymap.csv` / `storymap.mmd` | Derived by the skill's bundled scripts |
| `backlog.md` / `backlog.csv` | WSJF-ranked, 49 stories, dependencies, MoSCoW |
| `slice-1-acceptance-criteria.md` | Given/When/Then for the 15 slice-1 stories + INVEST check |
| `e2e-test-contract.md` | Coverage matrix, E2E-HAPPY journey, 12 scenarios, sequencing |

**Structure validated:** 49 stories (cap 50), 15 in slice 1 (cap 15), all 7 activities covered by slice 1, slice-1 dependency feasibility PASS, no dependency cycles.

## Mode note — this was **not** Mode D

There was no prior story map to extend. `docs/design/notebook-plugin-storymap.md`, `-backlog.*`, and the slice-1 criteria from the earlier session were **untracked scratch files and never entered git** (`git log --all` returns nothing for them). They are now gone from the working tree.

The authoritative prior artifact is the committed `docs/design/notebook-plugin.md`, which supplied the 5 researcher activities, the transport/payload contracts, and the decisions already made. This map preserves those and adds the platform columns. If anyone still has the old scratch files, they should be reconciled or discarded deliberately — right now they are lost, not superseded.

## The preference question — answered

You asked me to figure out the preference from the repo and comments. The short version:

**The notebook plugin is wanted. The TensorBoard service is not.**

- `chokevin` (maintainer): *"Overally I'm totally onboard with an interactive notebook."*
- `chokevin` on TensorBoard: *"orthogonal to how we want to display per job metrics… not the same format as offline W&B metrics which is what the industry seems to use today."*
- `chokevin` on approach: *"We do already have a metrics offload sidecar we should configure that container to handle the different metrics logging but again we should make sure it works as broadly as possible."*
- `chokevin` on evidence: *"I would need some analysis between the tensorboard format and the existing format."*

Corroborated in code: the house format **is** W&B-style (`_step`/`_timestamp`); `portal/README.md:52` declares the verb I referenced earlier **legacy** and names `taugrid-metrics-collector` as what new sidecars use.

**Two contradictions this creates.**

1. **C1 — the TensorBoard sidecar decision is now withdrawn.** The design we discussed earlier chose "run TensorBoard as a service, proxied by the portal." That conflicts with the maintainer's stated preference *and* with `GOVERNANCE.md:58`, which routes new required services through a design proposal. The map records it as withdrawn (D005) pending the format analysis, with `tfevents` becoming an *adapter* instead (S048 → S049). **If you still want the service, that is a decision to make explicitly against maintainer guidance, not a default.**
2. **C2/C3 — the plugin is wrong in two places the platform is already right.** Readiness is read from a field the CRD does not populate, and applicability is not enforced at all — while the portal and CLI both enforce it. Neither is a multi-tenancy feature; both are one policy implemented three ways.

## What is still uncertain

- **Q-01** — Does `taugrid-metrics-collector` gain `tfevents` parsing directly, or does a separate reader convert to house format first? Affects S049 only. Blocked on S048.
- **Q-02** — Is the "local TauGrid without ADX" path (`chokevin` raised it) in scope here or a separate map? Assumed out; activity 7's retention stories assume *some* durable store.
- **Q-03** — Who owns profile *applicability policy* — who declares namespaces/teams on a profile? Assumed platform-engineer writes, tenant-admin consumes.
- **S011 has two acceptable designs.** `feiskyer` offered both: execute the notebook on GPU-bearing compute, or restrict submissions to notebooks that dispatch GPU work through Ray. The map carries both; the choice is a design call, not a backlog call.

## Smallest next decision

**Decide the S011 shape** — execute on GPU compute, or restrict to Ray-dispatching notebooks. It is the only open question that changes what slice 1 even means, it is already the subject of a review comment, and everything in activity 2 downstream of S010 depends on the answer.

The second-smallest: accept or reject the withdrawal of the TensorBoard service (D005). That one is cheap to answer and closes a loop with the maintainer.

## Outstanding from earlier in this session — not resolved

While checking PR comments I found that on commit **`61c2f03`** three workflows failed, and **I only fixed one**:

| Workflow | Result on `61c2f03` | Status |
|---|---|---|
| TauGrid SDK Validation | failure | **Fixed** in `7c07fe4` (widgets extra + CI install) |
| TauGrid Kubernetes Validation | success | — |
| TauGrid Validation | failure | **Not investigated** |
| TauGrid Docs Validation | failure | **Not investigated** |

Also: the current head **`7c07fe4` is `action_required`** on all four workflows — the fork PR needs a maintainer to approve the workflow run before CI executes at all. So "the CI is red" may partly mean "CI has not run."

Two unread failure logs remain. That is the concrete loose end from the `fix ci` request.

## What this map deliberately does not cover

Per ROADMAP "Not planned" and decision D003: model registry, serving/inference, framework internals, data-preparation logic, and concurrent tenant isolation. Each would require a roadmap change under GOVERNANCE before becoming a story here.
