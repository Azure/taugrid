# nanoGPT Ray/Tau experiment

> **Status:** `smoke/debug` for the local and 1-GPU paths.
> **Intended use:** validate the Tau -> Kueue -> Ray path end to end with a real
> PyTorch training script, then scale the same script up behind your own
> `tau.yaml`.
> **Not for:** treating the synthetic fallback or inline FineWeb tokenization as
> production data prep.

This example turns a GPT-2-shaped causal LM run into a Tau-shaped PyTorch Ray
workload. It ships the training script and a result verifier; you supply a
`tau.yaml` describing the queue, storage, and compute shape for your cluster.

## Files

- `train.py` — self-contained PyTorch causal LM training script using Ray
  `TorchTrainer`.
- `verify_result.py` — checks metrics/history for CUDA, FineWeb source, loss
  decrease, and parameter count.

## Dataset contract

At runtime the script searches for FineWeb token shards under:

- `/data/datasets/fineweb/**/*.bin`
- `/data/datasets/fineweb/**/*.npy`
- `/data/datasets/fineweb/**/*.pt`
- `/data/datasets/fineweb/**/*.txt`

Binary `.bin` shards are read as `uint16` token ids, matching the common
llm.c/nanoGPT data shape. If no FineWeb shard exists, the script falls back to a
tiny deterministic byte corpus and records `"dataset_source":
"fallback:synthetic-byte-corpus"` in `metrics.json`. That fallback proves the
Tau path, not a data target.

Setting `NANOGPT_PREPARE_FINEWEB=1` lets the script create a GPT-2-tokenized
FineWeb smoke shard before training, on a cluster with egress. Prefer a
pre-tokenized dataset artifact for anything larger than a smoke run: inline
tokenization holds GPUs idle while it downloads and tokenizes. Split
tokenization into a CPU data-prep run and let training consume a pre-tokenized
manifest.

## Run locally

```bash
# from the repository root
python3 cli/examples/nanogpt-ray/train.py \
  --local-smoke --steps 2 --batch-size 2 --eval-every 1 --eval-batches 1 \
  --out-dir /tmp/nanogpt-smoke
```

This needs no Ray install and no cluster. It writes `metrics.json` and
`stellar/history.jsonl`, with `dataset_source` set to the synthetic fallback.

## Run on a cluster

Write a `tau.yaml` that points `entrypoint` at `train.py` and sets the queue,
storage, and compute shape for your cluster, then:

```bash
make install-tau-cli
tau run --config path/to/your/tau.yaml --dry-run=client
tau run --config path/to/your/tau.yaml
```

See [`../ray-tune-smoke/tau.yaml`](../ray-tune-smoke/tau.yaml) for a minimal
config-first example to copy from, and
[`docs/tau/tau-run-config.md`](../../../../docs/tau/tau-run-config.md) for
the full schema.

Keep one config per experiment variant rather than adding `tau run` flags: `tau run`
stays config-first, and each `tau.yaml` owns its entrypoint, runtime, storage,
queue, and metrics contract.

## Outputs

The run writes, under `/data/checkpoints/workflows/<run>/`:

- `metrics.json`
- `checkpoints/last.pt`
- `stellar/history.jsonl`

Import the Stellar-style history into an expstore:

```bash
taugrid-portal experiment import jsonl \
  --run nanogpt-fineweb-ray \
  --history /path/to/history.jsonl \
  --project nanogpt-smoke \
  --experiment nanogpt-fineweb \
  --group ray-smoke
```

## Scale knobs

Edit your `tau.yaml` rather than adding `tau run` flags:

- `runtime.env.NANOGPT_STEPS`
- `runtime.env.NANOGPT_BATCH_SIZE`
- `runtime.env.NANOGPT_BLOCK_SIZE`
- `runtime.env.NANOGPT_N_LAYER`
- `runtime.env.NANOGPT_N_HEAD`
- `runtime.env.NANOGPT_N_EMBD`
- `runtime.env.NANOGPT_USE_COMPILE`
- `runtime.env.NANOGPT_USE_BF16`
- `runtime.env.NANOGPT_USE_RMSNORM`
- `runtime.env.NANOGPT_ACTIVATION`
- `runtime.env.NANOGPT_COOLDOWN_FRAC`
- `runtime.env.NANOGPT_STOP_AT_TARGET`
- `compute.workers`
- `compute.gpus_per_worker`
- `policy.queue`
- `storage.data_pvc`

Increase batch/steps only after a smoke run proves queue admission, dataset
visibility, output import, and verifier success.

Verify a result bundle:

```bash
python3 cli/examples/nanogpt-ray/verify_result.py \
  --metrics /path/to/metrics.json \
  --history /path/to/history.jsonl \
  --require-cuda \
  --require-fineweb \
  --min-parameters 100000000
```
