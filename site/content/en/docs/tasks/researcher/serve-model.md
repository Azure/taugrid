---
title: Serve a trained model
weight: 2
description: Render, deploy, inspect, and remove an online endpoint
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

`tau serve` turns a project-owned image and optional durable checkpoint into a
[service](../../concepts/glossary/#service). Choose:

- `--kind=rayservice` (default) for a Ray Serve application; KubeRay must be
  installed and the image must expose the specified Python import path.
- `--kind=deployment` for a plain Kubernetes Deployment such as a raw vLLM,
  TGI, Triton, or custom HTTP server.

You need a platform-provided serving profile, a pinned image, the target
namespace/context, and a checkpoint visible from the serving PVC when the
endpoint loads model state.

Served workloads are admitted through Kueue. `tau serve` resolves the
platform-managed default LocalQueue from the target namespace and stamps it on
the workload; researchers do not select a queue. If the namespace has no usable
default LocalQueue, deployment fails with an onboarding error rather than
creating pods Kueue never admits.

## Render before deployment

Use a resolved checkpoint path for an offline client dry-run:

```bash
tau serve deploy <service-name> \
  --kind=rayservice \
  --profile <serve-profile> \
  --image <pinned-image> \
  --import-path serve:app \
  --checkpoint <checkpoint-path> \
  --checkpoint-pvc <pvc> \
  --namespace <namespace> \
  --context <context> \
  --dry-run=client
```

`--checkpoint` mounts the selected PVC at `/data`, resolves relative paths
under `/data/checkpoints`, and sets `TAU_MODEL_PATH`. Your application still
owns how it loads the model and handles requests.

## Deploy and inspect

Run the same command without `--dry-run=client`, then inspect the endpoint:

```bash
tau serve status <service-name> \
  --kind=rayservice \
  --namespace <namespace> \
  --context <context>
```

If a completed managed finetune or model-registry entry already records the
checkpoint, use `--from-finetune <run>` or `--from-model <model-ref>` instead
of `--checkpoint`. Those forms read Kubernetes metadata and therefore cannot
use client dry-run; use `--dry-run=server` or a live deployment.

For a non-Ray server, switch to `--kind=deployment`. Add `--deployment-port`
when the container listens on a port, and `--readiness-path` /
`--service-port` when the platform should render those contracts — a probe
path needs a port from `--service-port`, `--service-target-port`, or
`--deployment-port`.

## Scale or remove

Plain Deployments can be scaled directly:

```bash
tau serve scale <service-name> \
  --kind=deployment \
  --replicas 3 \
  --namespace <namespace> \
  --context <context>
```

Direct `scale` is not implemented for RayService because its Serve config is a
serialized field. Redeploy a RayService with `--replicas`, or use
`--min-replicas` and `--max-replicas` when creating it.

Remove either kind explicitly:

```bash
tau serve delete <service-name> \
  --kind=rayservice \
  --namespace <namespace> \
  --context <context>
```

Serving does not currently activate `tau/workspace.connection.yaml`; pass the
namespace and context supplied by the platform handoff. The project image must
provide the configured import path and all runtime dependencies.
