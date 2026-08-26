---
title: Retry and resume
linkTitle: Retry or resume a run
weight: 30
description: Recover transient failure while preserving root cause
url: "/docs/platform-admin-guide/recovery/"
aliases:
  - "/docs/operations/recovery/"
---

{{< maturity status="ga" reviewed="2026-07-16" >}}

TauGrid has two recovery paths for a failed [run](../../reference/glossary/#run):
automatic retry, driven entirely by the `resilience.*` fields in your
[run config](../../reference/run-config/), and manual `tau run resume`. TauGrid
performs retry as automatic behavior of `tau run` when
`resilience.max_retries > 0`; there is no separate `tau run retry` subcommand
to invoke.

## Automatic retry (`resilience.*`)

```yaml
resilience:
  max_retries: 2                    # default 0 (disabled)
  retry_on: ["Preempted", "Evicted"] # default; OOMKilled is opt-in
  backoff_initial: 30s               # default
  backoff_max: 5m                    # default
  checkpoint_path: /data/checkpoints/finetunes/<name>  # default, derived from run name
```

When `max_retries > 0` and you did not pass `--dry-run`, `tau run`:

1. Waits for terminal workload state.
2. Classifies the failure into one of: `OOMKilled`, `Preempted`, `Evicted`,
   `Completed`, `Running`, or `Unknown`.
3. Checks the failure reason against `retry_on` (case-insensitive) and the
   remaining attempt budget. **If the failure reason is not in `retry_on`,
   TauGrid does not retry: it exits with an error** naming the reason and the
   configured list, so an unexpected failure surfaces instead of looping
   silently.
4. Applies bounded exponential backoff: doubles from `backoff_initial`,
   capped at `backoff_max`.
5. Injects the checkpoint directory, attempt number, and failure reason into
   the next attempt's environment.
6. Deletes the failed workload and resubmits the same config.

If every attempt is exhausted, TauGrid exits with an error rather than leaving
the workload retrying indefinitely.

## OOM, preemption, and eviction are not interchangeable

`retry_on` defaults to `["Preempted", "Evicted"]`; both are transient,
infrastructure-driven states where the same config commonly succeeds on
resubmission. `OOMKilled` is deliberately **not** in the default list: the
same resources requesting the same memory usually reproduce the same OOM, so
automatically retrying it unchanged is opt-in. Add it to
`retry_on` only after you have adjusted `compute` (or the workload's memory
footprint), otherwise you are just spending queue time to fail the same way
again.

`Unknown` is never retryable, automatically or manually. If TauGrid cannot
classify the failure, that means the signal you'd need to decide "retry" or
"fix and resubmit" is missing: inspect `tau run status <name>` and `tau
run logs <name>` first rather than looping on an unclassified failure.

If retries keep exhausting for the same reason, check whether the cluster
itself is unhealthy before resubmitting again:
`tau cluster validate nodes` runs privileged GPU health probes across nodes.
See the [CLI reference](../../reference/cli/#tau-cluster) for its flags and
cluster-level troubleshooting.

## Manual resume

```bash
tau run resume <run-name> --config tau/train.yaml
```

`--config` is required: resume re-resolves the same direct run config used
for the original submission. TauGrid discovers the checkpoint directory (or
`--from` to override it), injects it into the new attempt, deletes the old
workload, and resubmits. **If the original failure was `OOMKilled`, `resume`
requires `--force`**, the same opt-in-only-after-you've-changed-something
reasoning as automatic retry applies here too, just gated by an explicit flag
instead of a config list.

Resume requires a durable checkpoint contract (a `storage` mount under
`/data`, not node-local scratch). If your workload only ever wrote
checkpoints to ephemeral storage, there is nothing to resume from: that
state vanishes once the workload is deleted.
