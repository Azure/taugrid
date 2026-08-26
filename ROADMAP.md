# TauGrid Roadmap

Where TauGrid is going.

This file describes direction. It is not a release plan, it carries no dates,
and no item creates a support commitment. Current behavior and supported
configuration are documented in the user guides and reference.

**Next** is the accepted next wave of work. **Exploring** is under
consideration, with no commitment that it will be built. Items are in rough
priority order, and work that has started is tracked in issues labeled
[`roadmap`](https://github.com/Azure/taugrid/issues?q=is%3Aissue+is%3Aopen+label%3Aroadmap).
Maintainers review this file each release cycle.

## Next

### Workspaces and multi-tenancy

Scoped identity, RBAC, quotas, shared queues, and an auditable record of
workspace changes. Today a cluster runs one active workspace, and researcher
isolation is still gated on negative-access tests.

### Portal experience

Grow the portal from a read-only view into a place where work happens: submit
and act on workloads from the cluster and workload views, with the same
workspace authorization the read paths already enforce.

### Dataset and data-preparation lifecycle

A public path through fetch, staging, validation, tokenization, registration,
and reuse, promoting the dataset registry past Alpha. Includes Hugging Face
credentials supplied once and reused across runs, and dataset replication that
is not driven by hand.

### Distributed training and fine-tuning

Validated end-to-end examples for PyTorch DDP, FSDP, DeepSpeed, and Hugging
Face LoRA or QLoRA, so framework choice is a project decision rather than a
platform integration project.

### Provider-agnostic observability and cost attribution

Workload health, alerts, and per-team cost attribution on any Kubernetes
cluster. Azure Data Explorer becomes one supported backend among several,
alongside other clouds and self-managed open-source stacks.

## Exploring

### Inference and serving

A released model-serving quickstart, plus production-shaped examples for vLLM,
SGLang, and TensorRT-LLM.

### Cross-cloud and heterogeneous execution

Extend deterministic MultiKueue profile dispatch beyond preconfigured workers:
secure worker credentials, portable data, images, checkpoints, and artifacts,
and routing that is not restricted to one cloud. On the capacity side, one
submission spanning CPU, GPU, and specialized nodes, with topology and
preemption decided by policy.

### Reinforcement learning and post-training

Extend the existing PufferLib path with validated Verl and OpenRLHF examples.

### Agent-driven research loops

Let an agent run the loop instead of a person: propose a configuration, submit
it, read the evidence, decide the next run. The portal already summarizes a
research loop; driving one is the open work.

## Not planned

TauGrid stays a workflow and lifecycle layer, as described in
[What is Tau?](https://azure.github.io/taugrid/docs/overview/what-is-tau/). It
does not intend to own:

- Cloud or Kubernetes cluster provisioning.
- Pod scheduling or quota decisions that belong to Kubernetes and Kueue.
- Ray, PyTorch, or model-framework internals.
- Model code, data-preparation logic, or serving application semantics.
- A replacement for, or fork of, the upstream components it integrates.

## Proposing a change

Open a [feature request](https://github.com/Azure/taugrid/issues/new/choose)
describing the user problem. Direction and public-contract changes start as an
issue or design proposal — see [GOVERNANCE.md](GOVERNANCE.md).
