# Serving models

Use `tau serve` for a long-running endpoint, not `tau run` lifecycle commands.
Choose `rayservice` for Ray Serve applications or `deployment` for a plain
HTTP server such as raw vLLM, TGI, Triton, or project code.

## Resolve the target before rendering

`tau serve deploy` requires an active repository workspace connection and an
explicit ready `--profile`. It resolves the live TauWorkspace namespace and
LocalQueue, validates queue access/ClusterQueue bindings, and selects the
applicable serving profile. Explicit namespace/context flags must match that
connection; they cannot redirect deployment to another workspace.

Even `--dry-run=client` uses this connected path. There is no serve snapshot
flag. First-time interactive connection review may be required. From a
monorepo, run in the intended project directory.

```bash
tau serve deploy <service-name> --kind=rayservice \
  --profile <serve-profile> --image <pinned-image> \
  --import-path serve:app --checkpoint <checkpoint-path> \
  --checkpoint-pvc <pvc> --dry-run=client
```

The image must contain the configured import path and compatible Ray/runtime
dependencies. If setting `--ray-version`, match the image; do not copy a
version from a different workload. Client rendering does not test imports,
pull the image, mount the PVC, or exercise the endpoint.

## Checkpoint paths

`--checkpoint` mounts the PVC root at `/data` and sets `TAU_MODEL_PATH`:

| Input | Runtime value |
| --- | --- |
| `projects/team/runs/train/checkpoints/last.pt` | `/data/projects/team/runs/train/checkpoints/last.pt` |
| `finetunes/train/checkpoints/last.pt` | `/data/checkpoints/finetunes/train/checkpoints/last.pt` |
| `/data/projects/team/runs/train/checkpoints/last.pt` | Unchanged |
| Any other absolute path | Unchanged; the image or another mount must provide it |

Other relative paths resolve under `/data/checkpoints`. A `..` component is
rejected. Tau does not infer the checkpoint from the active workspace or test
the filesystem during path resolution. Use the actual training output PVC;
`--checkpoint-pvc` defaults to `blob-training`, which may not be yours.

`--from-finetune`, `--checkpoint-ref`, and `--from-model`/`--model-ref` use
indexed metadata. They cannot use client dry-run; use a resolved path for
client preview, or server dry-run for metadata lookup. Choose one checkpoint
source, not competing flags. The application still owns loading model weights.

## Literal Deployment commands

For `--kind=deployment`, omit command/argument flags to retain image defaults.
Use repeatable `--command` and `--arg` for literal argv; use `--arg=<value>`
for arguments beginning with a dash:

```bash
tau serve deploy <service-name> --kind=deployment \
  --profile <serve-profile> --image <pinned-image> \
  --command python --arg serve.py --arg=--label --arg 'hello, world' \
  --deployment-port 8080 --readiness-path /health --service-port 8080 \
  --dry-run=client
```

Each value is one element; Tau performs no shell parsing. Explicit shell use
would be `--command /bin/sh --command -c --arg 'exec python serve.py'`, and
the image must contain that shell. Prefer dependencies baked into the image.

Legacy `--args` splits on whitespace and does not preserve shell quoting;
it conflicts with `--arg`. Do not use it for arguments containing spaces.
`--command`/`--arg` are rejected on RayService: KubeRay owns Ray startup.
Use `--import-path`, `--runtime-pip`, and non-secret `--env` for Ray Serve,
not a long-running application in the Ray head startup command.
Use `--env-secret KEY=SECRET:KEY` for Secret-backed settings.

Probe paths require a port from `--deployment-port`, `--service-port`, or
`--service-target-port`. `--service-port` enables a ClusterIP Service; it does
not by itself create public ingress, authentication, or HTTPS.

## Deploy, inspect, scale, remove

Remove dry-run only after deployment is authorized. Subsequent commands do
**not** share deploy's automatic workspace targeting; pass the exact target:

```bash
tau serve status <service-name> --kind=rayservice \
  --namespace <namespace> --context <context>
tau serve scale <service-name> --kind=deployment --replicas 3 \
  --namespace <namespace> --context <context>
tau serve delete <service-name> --kind=rayservice \
  --namespace <namespace> --context <context>
```

Direct scale supports Deployments only. For RayService, redeploy with
`--replicas`, or configure autoscaling with `--min-replicas` and
`--max-replicas`. Explicit `--replicas` conflicts with enabled autoscaling;
`--min-replicas`, `--target-qps`, and `--scale-down-delay` require
`--max-replicas`. GPU count defaults to the selected profile and `--gpus`
must agree with it.

After apply, distinguish resource readiness from a successful model request.
Report an endpoint as tested only after an authorized request actually
succeeds.
