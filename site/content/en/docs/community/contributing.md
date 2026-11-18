---
title: Contributing
weight: 1
description: Contributor expectations and validation depth
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

Start with the repository
[`AGENTS.md`](https://github.com/Azure/taugrid/blob/main/AGENTS.md)
and
[`SDK_GUIDE.md`](https://github.com/Azure/taugrid/blob/main/cli/SDK_GUIDE.md).

Core principles:

- Start from the user path and compatibility contract.
- Keep commands thin and capability packages cohesive.
- Reuse existing helpers and dependencies.
- Keep durable local formats inspectable and versioned.
- Preserve expstore as local truth.
- Make retry, checkpoint, and partial-write behavior explicit.
- Add tests at the owning layer.

External contract changes need external evidence. A manifest or telemetry change
needs evidence beyond a utility unit test.

## Required CI checks

The intended branch requirement contexts for pull requests to `main` are:

| Workflow | Exact required check context |
| --- | --- |
| TauGrid Validation | `TauGrid Core Required` |
| TauGrid SDK Validation | `TauGrid SDK Required` |
| TauGrid Image Validation | `TauGrid Images Required` |
| TauGrid Docs Validation | `TauGrid Docs Required` |
| TauGrid Kubernetes Validation | `TauGrid Kubernetes Required` |

Select these job check names, not the workflow names or the former ambiguous
`Required` context. Workflow changes alone do not enable branch protection:
maintainers must configure these contexts in the repository ruleset after
verifying that each reports on a pull request.

Observe these checks before making them required. Verify successful hosted pull
request runs, intentional skips for unrelated changes, and failure propagation
before enabling enforcement. Local tests or successful runs of older workflow
revisions are not proof that the new gates work. Do not require unpublished check
names, which would block existing pull requests until they adopt the new workflows.

All five workflows start on every pull request and push to `main`. Core and SDK
validation always run. The core gate includes all Go matrix jobs, Windows CLI,
Kueue host-slot validation, repository metadata and gate contracts, dependency
review on pull requests, and release preflight when selected.

Image, documentation, and Kubernetes workflows first check the changed paths
using `scripts/ci/validation-scope.sh`. Their expensive jobs retain their previous
path scopes; changes to the scope helper or its regression test select all three.
Pull requests use the merge-base diff; pushes use the complete before/after diff.
Renames and deletions count, including an old path moved out of a suite's scope.
An unavailable push base conservatively selects the suite. Detection errors fail
the gate rather than silently skipping validation.

Each conditional gate succeeds only when scope detection succeeds and every
selected job succeeds, or when the suite is unselected and every job is skipped.
Failed, cancelled, or unexpectedly skipped jobs cannot pass a selected gate.
Manual runs select the entire suite; existing schedules are unchanged. Scope
detection uses Git with read-only checkout access, not pull request APIs or
secrets, so it also works for fork pull requests.

Do not require individual path-filtered workflow checks (including Helm and
Terraform checks) globally: an unrelated pull request may never create those
checks. They remain scoped supplemental validation rather than part of these
five aggregate requirements. Merge queues are not configured by these workflows;
adding one also requires `merge_group` trigger and scope support.

Run the gate regression contract locally with:

```bash
bash scripts/ci/tests/required-gates-contract_test.sh
```

It also runs in core CI and the repository `make test` target.

## Dependency update sources

Portal frontend lockfiles must resolve public packages through
`https://registry.npmjs.org/`, not a package mirror. The frontend registry check
runs before every production build.

Helm dependency updates exclude `values.yaml` and `values.yml` before dependency
discovery. This avoids an upstream image-discovery defect that duplicates the
registry hostname in image lookup paths. Chart definitions and lockfiles remain
eligible for dependency updates; automated image-tag updates from Helm values
are intentionally paused. Review those tags during coordinated releases. Docker
base-image updates remain enabled independently. Remove the values-file exclusion
once the upstream Helm updater handles these image repositories correctly.

Run `bash scripts/ci/tests/dependency-sources-contract_test.sh` to check that
the exclusion preserves chart dependency updates.

## Publish a short blog post

Use the blog archetype to create consistent front matter:

```bash
cd site
make deps
./.bin/hugo new content --kind blog blog/<post-slug>.md
```

Replace the generated title and description, set `draft: false`, and write the
post in `content/en/blog/`. Keep detailed procedures in the task-oriented
documentation and link to them from the post. Run `make check` before opening a
pull request.
