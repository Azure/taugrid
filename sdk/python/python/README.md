# Tau Python SDK

Python SDK for Tau-managed workflows. For new distributed training code, the
default path is still plain Ray Train authoring plus a repo-local `tau.yaml`
submitted by the Go `tau` CLI:

```yaml
name: my-ft
engine: rayjob
entrypoint: train.py
runtime:
  pip: [torch==2.4.0]
policy:
  namespace: ray
  queue: dev
  gpu_class: any
```

```bash
tau run
```

Use this SDK when the project's reusable authoring unit is a Python handle:
local/remote execution, dynamic handle construction, multi-file source/wrapper
staging, job-scoped secrets, or the supported one-train-to-one-eval
checkpoint/model lineage. `tau.serve(...)` creates a separate handle from a
train/model reference; it is not discovered by `tau python submit`. The
SDK-generated manifest is an advanced renderer handoff, not the default
hand-authored config format; write a direct `tau run --config` file when the
reusable unit is a named repository target.

The train/eval/serve handles are stable first-class Python authoring APIs, not
experimental, deprecated, or fallback sugar. They are also not the scaffold
default for repository projects. The
[`Tau authoring strategy`](../../docs/tau/tau-authoring-strategy.md) defines
the objective selection, stability, parity, and migration contract.

`import tau` is the Python SDK API; `tau python ...` is the Go CLI proxy into
the same SDK. Keep Python focused on handles, authoring ergonomics, and local
developer workflows. The Go CLI remains the canonical executor for
Kubernetes, Kueue, RayJob submission, serving, expstore, and telemetry behavior.
The workload namespace default is `ray` after explicit config/context
resolution. Storage-sensitive examples pass `--namespace ray` explicitly when
they need to show the workload namespace.

Related design docs:

- [Tau authoring strategy](../../docs/tau/tau-authoring-strategy.md)
- [Tau API inventory and object model](../../docs/tau/tau-api-inventory-object-model.md)
- [Tau workload submission and scheduling contract](../../docs/design/tau-workload-submission-scheduling.md)
- [Tau SDK and Stellar HTTP API contract](../../docs/tau/tau-sdk-http-api-contract.md)
- [Tau telemetry naming and schema versioning](../../docs/tau/tau-telemetry-schema-versioning.md)

```python
import tau

@tau.train(name="my-ft", gpus=2, runtime_pip=["torch==2.4.0"])
def train(ctx):
    print(ctx.name, ctx.gpus)

train()          # local
# train.submit() # remote RayJob
```

Cluster scheduling belongs in decorator/config fields; Tau Python SDK writes
those into a temporary generated `schema_version: 1` manifest and delegates to
`tau run --config`:

```python
@tau.train(
    name="my-ft",
    gpus=1,
    gpu_resource_mode="device-plugin",
    node_selector={"gpu": "a100"},
    data_pvc="project-data",
    cpu_request=4,
    memory_request="32Gi",
    runtime_pip=["torch==2.4.0"],
)
def train(ctx):
    ...

train.submit(namespace="e2e-stack", dry_run="client")
```

`data_pvc` is the primary dataset/checkpoint PVC mounted at `/data` for train and
eval workloads. Use `tau.pvc_mount(...)` only for additional PVCs mounted at
non-reserved paths. For one-off submits, explicit `tau python submit` options can
override the decorator without editing source:

```bash
tau python submit experiments/vision_probe/config.py \
  --namespace ray \
  --context research-westus \
  --queue dev \
  --gpu-class any \
  --disable-default-priorities \
  --data-pvc lustre-research \
  --node-selector kubernetes.azure.com/agentpool=a10 \
  --cpu-request 4 \
  --memory-request 32Gi \
  --cpu-limit 4 \
  --memory-limit 32Gi
```

That shape keeps `VISION_ROOT=/data` while allowing cluster-specific placement
on node pools that can mount the selected PVC. Client dry-runs render offline;
live submits and server dry-runs fail fast when an explicit PVC's CSI driver is
not registered on every Ready GPU node still matched by the selected placement.
Resource request kwargs/flags write `compute.cpus` and `compute.memory`; if the
matching limit is omitted, Tau uses the request as the limit so small A10 nodes
do not inherit the legacy 16 CPU / 128Gi GPU limit.

Advanced experiment-specific config should live in the module/decorator manifest
instead of passthrough CLI flags:

```python
@tau.train(
    name="pretrain-dino",
    queue="dev",
    data_pvc="research-datasets",
    gpu_resource_mode="device-plugin",
    node_selector={"agentpool": "h200pool"},
    extra_manifest={"research": {"experiment": "demo-experiment"}},
)
def train(ctx):
    ...
```

RayJob training can opt into Nsight Systems profiling for Ray Train worker
ranks without editing rendered YAML. The runtime image must already contain
`nsys`; pass a temporary profiler image through `runtime.image` in the manifest
or decorator config until an official profiler image exists.

```python
@tau.train(
    name="pretrain-dino",
    data_pvc="research-datasets",
    node_selector={"agentpool": "h200pool"},
    profiler="nsys",
    profile_rank="all",
    profile_duration="2m",
)
def train(ctx):
    ...
```

`profile_rank` accepts a single global Ray Train rank (`0`), a comma-separated
rank set (`0,8`), or `all`. Artifacts are written on the `/data` PVC under
`/data/checkpoints/workflows/<run>/profile/` as `rank-<rank>.nsys-rep`, optional
`rank-<rank>.sqlite`, `rank-<rank>.metadata.json`, and
`rank-<rank>.summary.md`. For live RayJobs, `tau cluster profiler fetch
tau-<run> -n ray --artifact rank-0.summary.md` discovers the path from RayJob
annotations; after the RayJob is cleaned up, pass `--path
/data/checkpoints/workflows/<run>/profile --pvc research-datasets`. Use
`tau cluster profiler fetch tau-<run> -n ray --rank all --out ./profiles/<run>` to
download all rank-scoped artifacts while the RayJob annotations are still
available; add `--world-size 16` when fetching a deleted 16-rank run by
explicit path/PVC.

Ray Train wrapper compatibility covers both Ray `RunConfig` shapes currently
used by Tau runtime images. Ray 2.39 images, including the H200/RDMA runtime,
do not accept `RunConfig(worker_runtime_env=...)`; the wrapper relies on the
RayJob-level `runtime_env` in that case. Newer Ray versions that expose
`worker_runtime_env` still receive the same per-worker runtime environment.
In both cases, declare required worker packages in `runtime.pip`.

For plain PyTorch scripts, `tau.train(...)` can adapt an existing function
entrypoint without making the user write a Tau-aware wrapper:

```python
import tau

job = tau.train(
    name="vision-vitenc",
    gpus=1,
    workers=1,
    data_pvc="lustre-research",
    runtime_pip=["torch==2.4.0", "transformers", "scikit-learn"],
    entrypoint="scripts/train_probe.py:run_probe",
    entrypoint_kwargs={
        "train_jsonl": "/data/datasets/vision/jsonl/train-u-zero.jsonl",
        "eval_jsonl": "/data/datasets/vision/jsonl/valid-u-zero.jsonl",
    },
    extra_scripts=["scripts/vision_paths.py"],
)

job()          # local: imports scripts/train_probe.py and calls run_probe(...)
# job.submit() # remote: stages local scripts under /script and runs through Ray Train
```

`entrypoint` accepts `path.py:function`; omitting `:function` calls `main`.
Local files are staged into the job ConfigMap and invoked from `/script`.
Absolute paths that do not exist locally are treated as pre-mounted pod/PVC
paths and must be available on the head and worker pods. `entrypoint_args` and
`entrypoint_kwargs` must be JSON-serializable because Tau stores them in the
submitted manifest; set `entrypoint_pass_ctx=True` only when the target function
wants the Tau `ctx` as its first argument.

Install from a checkout:

```bash
make -C /path/to/taugrid/cli install
python -m pip install -e /path/to/taugrid/sdk/python/python
tau python doctor -n ray
```

Pinned installs and troubleshooting:
[`../../../site/content/en/docs/getting-started/install.md`](../../../site/content/en/docs/getting-started/install.md).

Public API: `tau.train`, `tau.eval`, `tau.serve`, `tau.config`,
`tau.secret_from_env`, `tau.secret_from_file`, `tau.secret_ref`,
`tau.pvc_mount`, `tau.dataset_file_reference`,
`tau.read_jsonl_objects`, `tau.load_staged_module`,
`tau.load_staged_function`, `tau.call_staged_function`, `tau.stellar`.

## Dataset and staged-script helpers

Examples often need small data-preparation utilities before the actual train or
eval step starts. Keep domain-specific parsing in the example, but use Tau's
generic helpers for the repeated platform contracts:

```python
from pathlib import Path

import tau

rel_path, image_path = tau.dataset_file_reference(
    Path("/data/datasets/vision"),
    row["Path"],
    field_name="image path",
)
records = tau.read_jsonl_objects("/data/datasets/vision/train.jsonl", max_records=512)

summary = tau.call_staged_function(
    "/data/examples/vit-enc-vision/scripts/vision_to_jsonl.py",
    "convert_csv",
    csv_path=Path("/data/datasets/vision/train.csv"),
    image_root=Path("/data/datasets/vision"),
    output_path=Path("/data/datasets/vision/jsonl/train-u-zero.jsonl"),
    split="train",
    policy="u-zero",
)
```

`dataset_file_reference` rejects empty, absolute, and root-escaping manifest
paths, returning both a portable POSIX relative path and the resolved local path.
`read_jsonl_objects` skips blank lines, requires JSON objects, supports an
optional record cap, and reports `path:line` errors. The staged-script helpers
temporarily put the script directory on `sys.path` so sibling imports work, then
restore `sys.path`; pass absolute PVC paths from remote Tau jobs so Ray workers
do not depend on process working directories. Prefer `tau.train(entrypoint=...)`
for train-job entrypoints; use these helpers when a Tau-aware train/eval
function needs to call additional staged utilities.

Frozen-backbone linear-probe workflows are a valid example pattern, but the
model-specific feature extractor, label policy, and probe math should stay in
the downstream example. Tau owns the generic path, JSONL, staged-entrypoint, and
experiment logging contracts rather than ViT-Enc/Vision science code.

## Job-scoped secrets

Prefer source-backed job secrets when a run needs credentials such as Hugging
Face tokens:

```python
@tau.train(
    name="captioner-smoke",
    gpus=1,
    runtime_pip=["torch==2.4.0", "transformers"],
    env={
        "HF_TOKEN": tau.secret_from_env("HF_TOKEN"),
        "HUGGING_FACE_HUB_TOKEN": tau.secret_from_file(
            "HF_TOKEN",
            path="~/.cache/huggingface/token",
        ),
    },
)
def train(ctx):
    ...
```

At submit time Tau Python SDK reads the local environment variable or file, writes a
temporary `0600` payload file, and passes only that file path to the Go Tau CLI.
Go Tau applies a generated Kubernetes `Secret` together with the ConfigMaps and
Job/RayJob, and the pod env uses `valueFrom.secretKeyRef` to read the generated
Secret key. `--dry-run=client` redacts the generated Secret value.

This keeps token values out of Python manifests, command-line arguments, normal
stdout/stderr, and client dry-run output. After submit, the value is still a
Kubernetes Secret in the target namespace, so the effective protection is the
cluster's Secret RBAC, audit policy, and etcd encryption posture. Users with
permission to read Secrets in that namespace can read it. `--dry-run=server`
also sends the real Secret object to the API server for validation.

`tau.secret_ref("existing-secret", "key")` remains available for Secrets that
are intentionally managed outside Tau, but those Secrets must already exist in
the target namespace.

Future hardening options:

- Reject or require explicit opt-in for `--dry-run=server` when source-backed
  secrets are present.
- Avoid the local temp payload file by using an stdin/memfd-style handoff to the
  Go CLI.
- Add owner references or cleanup policy for generated job Secrets.
- Support external secret providers such as Key Vault/External Secrets Operator
  for clusters that should never receive raw tokens from the submit host.
- Add cluster-profile checks for Secret RBAC and etcd encryption before enabling
  source-backed job secrets by default.

## Stellar experiment logging

`tau.stellar` mirrors the W&B training-loop integration point: log scalar
metrics by key over `step`, log media/report artifacts separately, then sync the
local run packet into a Tau expstore.

```python
from tau import stellar

run = stellar.init(
    project="radiology-foundation-models",
    run="captioner-lora-v1",
    group="captioner-lora",
    experiment="img-captioner-workflow-validation",
    store="/mnt/expstores/img-captioner",
    config={"lr": 2e-5},
)

for step, batch in enumerate(loader):
    loss = train_step(batch)
    stellar.log({
        "train/loss": loss,
        "val/radgraph_f1": radgraph_f1,
        "retrieval/recall_at_5": recall5,
    }, step=step)

stellar.log({
    "examples/grounding": stellar.Image("grounding-overlays.png"),
    "examples/report_diffs": stellar.Html("report-diffs.html"),
}, step=step)
stellar.finish(sync=True)
```

The local packet is W&B-shaped (`history.jsonl` plus artifact files). Sync uses
`taugrid-portal experiment track` for configs/artifacts and
`taugrid-portal experiment import jsonl` for scalar history, so Stellar
dashboards can render `train/*`, `val/*`, `retrieval/*`, `embedding/*`, and
qualitative artifacts without a W&B service dependency.

Logging is local-first and needs no CLI at all. Only `sync=True` shells out, and
it uses the `taugrid-portal` binary — Stellar ships separately from `tau`.
Install it with `make install-taugrid-portal`, or point at one explicitly with
`TAUGRID_PORTAL_BINARY=/path/to/taugrid-portal` (or `stellar.init(tau_binary=...)`).

### PyTorch Lightning migration from W&B

For Lightning training loops, replace `WandbLogger` with
`stellar.StellarLogger`. The logger is local-first like `stellar.init`: metrics
from `self.log(...)` go to `history.jsonl`, hyperparameters go to `config.json`,
and checkpoint callbacks are recorded as `checkpoint/*` artifacts on finalize.
Install the optional Lightning integration only in environments that need it:

```bash
pip install -e ".[lightning]"
```

```python
from lightning.pytorch import Trainer
from lightning.pytorch.callbacks import ModelCheckpoint
from tau import stellar

stellar_logger = stellar.StellarLogger(
    project="radiology-foundation-models",
    name="captioner-lora-v1",
    group="captioner-lora",
    experiment="img-captioner-workflow-validation",
    store="/mnt/expstores/img-captioner",
    config={"lr": 2e-5, "rank": 16},
)

checkpoint_callback = ModelCheckpoint(monitor="val/radgraph_f1", mode="max")
trainer = Trainer(logger=stellar_logger, callbacks=[checkpoint_callback])
trainer.fit(model, datamodule=data)
```

For W&B-style validation samples, log a table or media artifact at the current
global step from a Lightning callback:

```python
class LogValidationSamples:
    def on_validation_epoch_end(self, trainer, pl_module):
        trainer.logger.experiment.log(
            {
                "examples/generated_reports": stellar.Table(
                    report_rows,
                    caption="Validation report examples",
                )
            },
            step=trainer.global_step,
        )
```

Captioned media and tables are synced through `taugrid-portal experiment track` artifact
metadata and step-scoped filenames such as
`examples-generated_reports-step-120.json`, so Stellar can group/filter output
media by run and step. Table artifacts keep their columns, preview rows,
caption, and step for API/UI preview.

Artifacts also carry a lightweight lineage contract. Ordinary logged artifacts
are `direction=output`; `use_artifact(...)` resolves a prior output by alias,
artifact ID, or name from the local expstore, copies it into the new run packet,
and records it as `direction=input` with `source_artifact_id` and
`source_run_id`:

```python
with stellar.init(project="radiology", run="resume-v2", store="/mnt/expstores/img-captioner") as run:
    ckpt = run.use_artifact("best")
    model = LitModule.load_from_checkpoint(ckpt)
```

`StellarLogger` also attaches local reproducibility context to the sync call:
Python runtime metadata, installed package versions, and the local Stellar
history path as a log pointer. Full W&B SaaS features such as hosted sweeps and
collaborative table querying remain outside this native Stellar workflow; keep
using W&B import/interoperability when those hosted features are the primary
requirement.

### Smart metadata defaults

When `group` or `experiment_id` are not explicitly provided, `stellar.init()` uses
environment variables for better experiment organization:

- **`group`**: Uses `TAU_GROUP` env var if set; otherwise uses the first segment
  of `JOB_NAME` (e.g., `vision` from `vision-training-20250616-abc`); falls
  back to `"default"` if neither is set.
- **`experiment_id`**: Uses `TAU_EXPERIMENT_ID` or `TAU_EXPERIMENT` env vars if
  set; otherwise derived from the `experiment` name; falls back to the
  `project` value if neither is provided.

This allows Ray/Kueue job specs to set metadata cluster-side without changing
training scripts:

```yaml
env:
  - name: TAU_GROUP
    value: "vision-vit-enc-ablation"
  - name: TAU_EXPERIMENT_ID
    value: "vit-enc-pretrain-validation"
```

Explicit parameters always override env vars. For existing runs with generic
metadata, replay or re-emit corrected metric projections; hosted Stellar uses
the canonical Kusto identity and does not apply mutable grouping overrides (see
`docs/tau/stellar-ui.md`).

Commands: `tau python inspect train.py`,
`tau python build train.py --output dist/tau-build`,
`tau python submit-build dist/tau-build`,
`tau python submit train.py -n ray`, `tau python bootstrap --tag v0.1.0`,
and `tau python doctor -n ray`.
Direct `python -m tau.cli ...` execution remains available for local SDK
debugging.

### Inspect, build, and dry-run

| Surface | What it proves |
| --- | --- |
| `tau python inspect MODULE` | Semantic train/eval intent before source staging or command-line overrides |
| `tau python build MODULE --output DIR` | Byte-stable generated production intent after staging and explicit build overrides |
| `tau python submit-build DIR --dry-run=client` | The final Kubernetes workload rendered by Go Tau from the verified artifact |
| `tau python submit MODULE --dry-run=client` | The same final rendering path without retaining a build directory |

The build directory is a generated `tau.python.build` v1 artifact, not a
hand-authored direct config:

```text
dist/tau-build/
├── tau-build.yaml
└── workloads/
    ├── train-<name>/
    │   ├── tau.yaml
    │   ├── tau_py_wrapper.py
    │   └── tau_user_module.py
    └── eval-<name>/
        └── ...
```

`tau-build.yaml` records the Tau Python SDK version, ordered workload
metadata, train/eval checkpoint lineage, staged-file roles, byte sizes, and
SHA-256 digests. The artifact has no timestamps or inferred checkout paths;
identical source plus identical flags produces identical bytes. Existing
output is refused unless `--force` is supplied.

Kubernetes `secretKeyRef` entries stay as references. For
`tau.secret_from_env` and `tau.secret_from_file`, the build records only the
environment-variable or file locator and rewrites the workload to a
deterministic generated Secret reference. `submit-build` verifies every staged
path, size, and digest, reads those local sources, creates the existing
temporary `0600` payload, and then delegates to `tau run --config`. Secret
values never enter the build directory.

The v1 build supports at most one train and one eval handle per module. A
sibling eval records deterministic checkpoint lineage from the train handle;
an eval-only build requires `--upstream-checkpoint`. `tau.serve` remains
intentional separate: mixed modules record excluded Serve handles, while
serve-only builds direct callers to `ServeHandle.deploy()` or
`tau serve deploy`.

Tests:

```bash
pip install -e ".[dev]"
pytest tests/
```
