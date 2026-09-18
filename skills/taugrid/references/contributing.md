# Contributing and maintaining this skill

Read the checkout's `AGENTS.md` and relevant component guidance first. The
source tree is a multi-module Go repository with a Python SDK and React
frontend, not just three Go modules.

| Path | Responsibility |
| --- | --- |
| `cli/` | `tau`, `tau-gen`, command routing, rendering, submission, lifecycle |
| `core/` | Shared run schema, profiles, status, metadata, Kubernetes clients |
| `portal/` | `taugrid-portal`, expstore, APIs, embedded frontend |
| `portal/frontend/` | React/TypeScript UI; rebuild embedded assets for UI changes |
| `controllers/tau-core/` | TauCluster, TauWorkspace, and quota reconciliation/CRDs |
| `monitoring/gpu-health-checker/`, `monitoring/gpu-metrics-collector/` | Independent Go modules |
| `tests/e2e/` | Portable tests; `AI_RUNTIME_E2E=0` keeps cluster tests disabled |
| `sdk/python/python/` | Optional independently versioned Python SDK |
| `site/content/en/docs/` | User documentation; verify against source |

CLI and Portal use a Go module replacement pointing at `../core`, not
`../taucore`. Use `.go-version` and component Makefiles as the toolchain
authority. `core/workloadmeta` owns shared `tau.azure.com/*` constants; avoid
duplicating literals in production code. Regenerate CRDs with controller
`make manifests`, never hand-edit generated output.

## Evidence map

| Skill contract | Source / regression coverage |
| --- | --- |
| Roots, run flags, target vs subcommand | `cli/internal/cli/command_tree_test.go`, `run.go`, `run_config_commands.go` |
| Direct field statuses and engine validation | `core/runconfig/config.go`, `schema.go`, `cli/internal/cli/run_config.go` |
| Authoritative profiles and snapshot-only offline mode | `core/resourceprofile/`, `cli/internal/cli/run_workload_profile.go`, `run_workload_profile_test.go` |
| Connection trust and workspace target | `cli/internal/workspaceconnection/`, `cli/internal/cli/active_workspace.go`, `workspace_connection_test.go` |
| Create/adopt and PVC warning semantics | `cli/internal/workspace/`, `cli/internal/cli/workspace_create.go`, `workspace_adopt.go`, `workspace_data_pvc_test.go` |
| Workspace phase and quota mutation | `controllers/tau-core/internal/controller/conditions.go`, `quota_request_controller.go` |
| Serve workspace binding, checkpoint paths, literal argv | `cli/internal/cli/serve.go`, `serve_target.go`, `serve_test.go` |
| Historical/discovered logs | `cli/internal/cli/run_logs_discovery.go`, `run_logs_connection_test.go`, `run_logs_missing_run_test.go` |
| SDK-generated configs and serving wrappers | `sdk/python/python/tau/workloads.py`, `cli.py`, `serve.py` |

## Validate without changing the user's tools

Build into the checkout rather than installing over a user's binary:

```bash
make -C cli build
cli/bin/tau version
cli/bin/tau run explain-config
python3 skills/taugrid/scripts/check_examples.py --tau cli/bin/tau
python3 scripts/check-license-headers.py
git diff --check
```

The skill checker needs Python 3.10+, PyYAML, and Git. It checks bundled Markdown
links and command/flag examples using help only, validates every complete
direct YAML example, and stages stub scripts in a temporary directory. These
default checks do not render workloads and do not depend on local test files.
The checker never submits workloads or executes the training scripts.

For rendering checks, pass an explicit `--snapshot <fixture-file>`. The fixture
must contain the illustrative `cpu`, `training-1gpu`, and `training-2x8`
profiles with their documented cardinalities and authorize namespace
`skill-tests`, team `research`, and lane `training`. Metrics examples receive
a synthetic workspace identity because snapshot mode skips live resolution.
The checker reports validation and rendering counts separately; it does not
prove image compatibility, training behavior, live profiles, connected serving,
or storage. Optional SDK proxy flags and portal commands are not executed.

The Go CI job runs the default checks against a freshly built CLI, so
command/schema changes can catch stale examples. Rendering still needs a
separately supplied fixture. Do not treat a parser-only check or an empty
example set as rendering proof.

Run component tests for behavior being changed:

```bash
cd cli
go test -count=1 ./internal/cli
```

For broader changes use the repository's `make check`; it forces offline
portable E2E. Live cluster tests, image builds/pushes, installation, and
deployment require separate authorization. An offline render is not runtime
proof, but a documentation task is not permission to submit a real GPU job.

## Review the skill as behavior, not a copied manual

Keep the entrypoint short and route conditional details to references. Cover
real tasks: offline rendering, CPU evaluation, multi-node DDP, Ray project
shipping, workspace adoption, literal serving arguments, and historical logs.
Distinguish schema acceptance from dispatch, live readiness, and execution.

When source and site documentation disagree, identify the precise source/test
contract. Fix documentation within the requested scope and flag any remaining
product inconsistency; do not repeat a remembered bug as current fact.
The local helper tests are not independent agent usability evaluation.
