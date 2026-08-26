---
title: What is Stellar?
linkTitle: What is Stellar?
weight: 4
description: How Stellar turns saved run data into experiment views and comparisons
---

Stellar is TauGrid's experiment tracking and comparison experience. It ships in
the `taugrid-portal` binary and presents run details, scalar metric history, and
side-by-side comparisons in a browser or terminal.

## What Stellar helps answer

- Which code and settings produced this run?
- How did loss, accuracy, throughput, or another metric change over time?
- Which run performed best?
- Where are the model, checkpoint, and other output files?
- Did the run finish, retry, or resume?

## Where the data comes from

TauGrid saves complete metric files and a small experiment index close to the
run. Stellar can read that saved evidence directly. A platform can also provide
an ADX/Kusto view for searching and comparing runs across workspaces.

adx-mon can move metric samples into ADX, while Stellar reads and presents the
result. The saved run files remain available for retrieval and detailed
inspection.

## Stellar and the Ray dashboard

The Ray dashboard shows live tasks, actors, and workers while a Ray cluster is
running. Stellar shows experiment evidence that remains useful after the
runtime pods have finished.

Open a local dashboard with:

```bash
taugrid-portal experiment stellar <run-name>
```

Try [Live experiment evidence](../../../examples/experiment-evidence/) to
publish loss and accuracy, retrieve output files, and open the run in Stellar.
