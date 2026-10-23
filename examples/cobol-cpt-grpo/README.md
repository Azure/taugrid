# COBOL continued pretraining, GRPO, and eval

> **Status:** `production-shaped`
> **Intended use:** adapt `Qwen/Qwen2.5-Coder-32B` to COBOL with continued
> pretraining (CPT), then GRPO with a compiler reward, and score the result on
> compile-checked COBOL benchmarks, on 2 x 8 H200 nodes with RDMA.
> **Not for:** first-time onboarding, CPU-only or single-GPU clusters, or
> serving. Start with [`nanogpt-ray`](../nanogpt-ray/) or
> [`ray-tune-smoke`](../ray-tune-smoke/) to validate a workspace first.

The pipeline has seven stages. Each stage folder has its own `tau.yaml`.
CPU stages run as Jobs; GPU stages run as RayJobs that ship the stage
folder as the Ray `working_dir`.

| Stage | Engine | What it does |
| --- | --- | --- |
| [`01-collect/`](./01-collect/) | Job (CPU) | Clones COBOL repositories and downloads public Hugging Face datasets into `/data/datasets/cobol/raw`. `tau-rl.yaml` collects the parser and test corpora used for RL prompts. `tau-gated.yaml` downloads gated datasets and needs `HF_TOKEN`. |
| [`02-data-prep/`](./02-data-prep/) | Job (CPU) | Extracts, deduplicates, filters, and splits the corpus into `prepared/train.jsonl` and `prepared/val.jsonl`. |
| [`03-benchmarks/`](./03-benchmarks/) | Job (CPU) | Builds five eval suites from the validation split into `/data/datasets/cobol/eval`. |
| [`04-cpt/`](./04-cpt/) | RayJob, 2 x 8 GPUs | Continued pretraining with DeepSpeed ZeRO-3 under Ray Train. |
| [`05-rl-prompts/`](./05-rl-prompts/) | Job (CPU) | Masks paragraph bodies of programs that compile with `cobc` to produce `prepared/rl-prompts.jsonl`. Programs that share any 8-line window with the validation split are excluded. |
| [`06-grpo/`](./06-grpo/) | RayJob, 2 x 8 GPUs | GRPO from the CPT checkpoint with a compiler reward and an in-cluster vLLM generation server. |
| [`07-eval/`](./07-eval/) | RayJob, 1 x 8 GPUs | Runs the benchmarks with vLLM (TP=8) and compiles every generated program with GnuCOBOL. |

Serving the trained model is not part of this example; no `08-serve` stage is
included.

## Data sources and licensing

`01-collect/collect-cobol-data.sh` and `01-collect/collect-rl-data.sh` read
`SOURCE_LICENSES`:

- `permissive` (the checked-in default) clones only repositories whose GitHub
  license is MIT, Apache-2.0, BSD, CC-BY-4.0, or US-government public domain:
  - CPT corpus: `aws-samples/aws-mainframe-modernization-carddemo`
    (Apache-2.0), CobolCraft (MIT), the openmainframeproject COBOL course
    (CC-BY-4.0), Cobol-Programming-Collection (MIT), shamrice (MIT), koopa
    (BSD), pc-pricer (US government, public domain), and the exercism COBOL
    track (MIT).
  - RL corpora: proleap (MIT), cobol-rekt (MIT), Ironclad (Apache-2.0), DSF
    (MIT), Bank-of-Z (Apache-2.0), and FPS.cob (Apache-2.0).
- `all` also clones repositories that are copyleft (GPL-2.0, GPL-3.0,
  EPL-2.0, CECILL-2.1) or have no stated license. It also enables the NIST
  COBOL85 suite extracted from opensourcecobol4j and the zorse dataset. Use
  `all` only if your use of the trained weights complies with those terms.

The ungated Hugging Face datasets in `collect-cobol-data.sh` are
`TroyDoesAI/Cobol`, `bigcode/commitpack` (COBOL subset),
`Fsoft-AIC/MainframeBench`, `msplits/human-eval-cobol`, and
`harshini-kumar/CobolCodeBench`. Check each dataset card's terms before use.

Gated datasets are opt-in. They are `bigcode/the-stack` and
`bigcode/the-stack-v2` (COBOL subsets) and `4sp1d3r2/cobol-pretraining-corpus`.
Accept their terms on Hugging Face, then store a read token in a Secret in the
workspace namespace:

```bash
kubectl create secret generic hf-token --from-literal=token=<your-hf-read-token> \
  --namespace <workspace-namespace>
```

`01-collect/tau-gated.yaml` maps that Secret to `HF_TOKEN` with
`env_secret: {HF_TOKEN: hf-token:token}`. `collect-rl-data.sh` skips its gated
steps when `HF_TOKEN` is unset. Both collect scripts exit nonzero if any step
that runs fails, including a clone, or if no COBOL files are collected. To include them, add the same `env_secret`
block to `01-collect/tau-rl.yaml`. `02-data-prep` picks up the gated outputs
when they are present under `raw/`.

## Method

**CPT (`04-cpt`).** `driver.py` runs on the CPU-only Ray head and starts a Ray
Train `TorchTrainer` with 16 workers, one per GPU. Each worker calls
`continued_pretrain.main()`, which runs HF Trainer with DeepSpeed ZeRO-3 on
packed 4096-token chunks. By default that is 3 epochs at LR 2e-5, an effective
batch of 4 x 4 x 16, and eval every 25 steps. Ray Train sets up
`torch.distributed` and the rank environment that torchrun would. HF Trainer
owns checkpointing and resume under `OUTPUT_DIR`; Ray Train stores only its
run metadata under `OUTPUT_DIR/ray`. `final/` holds the trained model as 5 GB
safetensors shards.

**GRPO (`06-grpo`).** One RayJob runs both halves of TRL's server mode on the
two GPU workers:

1. A `VLLMServer` actor reserves all 8 GPUs of one worker and runs
   `trl vllm-serve` (TP=8). The driver waits for `/health` on `VLLM_PORT`
   (8000).
2. A `TorchTrainer` with 8 workers lands on the other node. Each worker sets
   `VLLM_SERVER_URL` and calls `train_grpo.main()`.

TRL syncs updated weights from trainer rank 0 to the server over an NCCL group
on TRL's group port (51216 in TRL 1.13.0). Stage 05 records whether each prompt
compiles as free or fixed format; GRPO keeps that `compile_fmt` and uses the
matching `cobc` flag for both the prompt preflight and the reward. The reward
has two parts:

- **Compile reward.** Each prompt plus completion is checked with
  `cobc -fsyntax-only` and `-free` or `-fixed`. A clean compile scores 1.0 and
  a compile with warnings scores 0.7. A `cobc` signal crash counts as a failed
  compile; a timeout or a missing `cobc` raises.
- **Structural reward.** Penalizes degenerate completions (display-only
  bodies, no-op majorities, repeated statements, truncation). It adds a bonus
  when every `PERFORM` target is a paragraph defined in the completion.

The defaults are K=4 samples per prompt at temperature 0.8, LR 5e-7, KL
coefficient 0.05, and 3 epochs. Every trainer rank checks the KL and
reward-hacking alarms, and the ranks combine their stop decisions so that all
of them stop at the same step. Only rank 0 publishes metrics.
`06-grpo/test_reward.py` covers the reward without a GPU.

**Eval (`07-eval`).** One Ray task reserves a whole 8-GPU worker and runs
`run_eval_vllm.main()` with greedy decoding (512 new tokens). It scores five
suites:

- Perplexity on held-out programs.
- Paragraph completion and program completion: BLEU, edit distance, and
  compilation.
- Structural validation: structure checks and compilation.
- Copybook usage: BLEU and edit distance only.

Compile scoring is format-aware: each generation is compiled with `cobc -free`,
retried with `-fixed` if that fails, and both counts are reported. A paragraph
completion is cut at the first paragraph header it starts. It is then compiled
with the rest of the original procedure appended, so a `PERFORM` of a later
paragraph resolves; BLEU and edit distance score only the cut completion. The
headline compile score sums the paragraph, program, and structural tasks.
Stage 03 caps these at 200, 50, and 100, so the total depends on the corpus.

Many source programs do not compile on their own, for example because a
copybook is missing. Each compile suite therefore also compiles the reference
in place of the generation: the paragraph prompt, reference, and appended
procedure, or the original program for program and structural tasks.
`reference_compiled` counts the tasks that compile that way, and
`compiled_where_reference_compiles` counts the generations that compile on
those tasks.

Before loading the model, the eval checks that `cobc --version` works and that
all five suite files exist, are not empty, and have the fields the current
stage 03 writes. It fails if any check fails, and it fails if `cobc`
disappears during the run. Rerun `03-benchmarks` after updating this example.
It also
tokenizes every generation prompt and sets aside any prompt that cannot fit in
`MAX_MODEL_LEN` (32768) together with its output budget. Set-aside task IDs are
listed under `excluded_over_context` in `eval-summary.json`, and the run fails
if a suite has no task left.

## Hardware

- CPT and GRPO: 2 GPU workers x 8 H200, `rdma.enabled: true`, and
  `policy.topology: same-network-domain`. The NCCL settings in
  `04-cpt/tau.yaml` assume InfiniBand HCAs named `mlx5*` and a pod interface
  `eth0`.
- Eval: 1 GPU worker x 8 H200, no RDMA.
- CPU stages: 1 to 8 vCPUs and 4 to 32 GiB memory.
- Storage: a writable workspace PVC mounted at `/data`. Datasets live under
  `/data/datasets/cobol`; checkpoints and eval output live under
  `/data/projects/taugrid-default/runs/cobol-cpt-grpo`.

If `/data` is a blobfuse2 mount, `train_grpo.py` writes safetensors through a
Python writer because the Rust `serialize_file` path fails on blobfuse2. For
the same reason, `continued_pretrain.py` saves with `max_shard_size="5GB"`.

## Substitute placeholders

The checked-in values are placeholders for a platform-specific copy of the
configs:

| Value | Where | Replace with |
| --- | --- | --- |
| `registry.example.com/cobol-train:v0.28.0-ray2.56.0` | `05-rl-prompts`, `04-cpt`, `06-grpo`, `07-eval` | An image you build from [`Dockerfile`](./Dockerfile) (vLLM 0.28.0, GnuCOBOL 3.1.2, and the pins in `image-requirements.txt`) and push to your registry. Reference it by digest. |
| `policy.workspace: taugrid-default` | every `tau.yaml` | Your TauWorkspace. |
| `storage.data_pvc: workspace-data` | every `tau.yaml` | The workspace's writable PVC. |
| `/data/projects/taugrid-default/runs/cobol-cpt-grpo/...` | `OUTPUT_DIR`, `MODEL_ID`, `OUT`, `storage.output` | Your workspace output root. |
| `policy.topology` | `04-cpt`, `06-grpo` | The placement of the workload profile you target. Set `policy.profile` if more than one ready profile matches; list them with `tau cluster profiles export`. |
| `TAU_METRICS_OFFLOAD_*` | environment of the `06-grpo` submitter | See below. |

The CPU stages use `python:3.11.17-bookworm` pinned by digest, from Docker
Hub. It includes `git`, `curl`, `unzip`, and `file`, which the collect
scripts need. The training image is based on the upstream
`vllm/vllm-openai:v0.28.0` image from Docker Hub, pinned by digest. Both are
exceptions to the MCR base-image preference; mirror them into your registry
if your platform requires it.

`06-grpo/tau.yaml` enables the Stellar metrics offload sidecar for
`metrics-history-attempt-0/*.jsonl`. Provide the collector image and ADX
settings, as in [`portal-ray-stellar`](../portal-ray-stellar/):

```bash
export TAU_METRICS_OFFLOAD_IMAGE=<taugrid-metrics-collector@sha256:digest>
export TAU_METRICS_OFFLOAD_ADX_CLUSTER_URI=https://<cluster>.<region>.kusto.windows.net
export TAU_METRICS_OFFLOAD_ADX_DATABASE=<database>
export TAU_METRICS_OFFLOAD_ADX_CLIENT_ID=<workspace-workload-identity-client-id>
export TAU_METRICS_OFFLOAD_OUT=/var/run/tau/metrics-offload
```

To run without Stellar, remove the `metrics.offload` block.

## Run it

Run from the repository root. Render each stage with a client dry-run before
submitting it, and wait for each stage to finish before starting the next.

```bash
make install-tau-cli

tau run --config examples/cobol-cpt-grpo/01-collect/tau.yaml --dry-run=client
tau run --config examples/cobol-cpt-grpo/01-collect/tau.yaml
tau run --config examples/cobol-cpt-grpo/01-collect/tau-rl.yaml
tau run --config examples/cobol-cpt-grpo/01-collect/tau-gated.yaml

tau run --config examples/cobol-cpt-grpo/02-data-prep/tau.yaml
tau run --config examples/cobol-cpt-grpo/03-benchmarks/tau.yaml

tau run --config examples/cobol-cpt-grpo/04-cpt/tau.yaml --dry-run=client
tau run --config examples/cobol-cpt-grpo/04-cpt/tau.yaml

tau run --config examples/cobol-cpt-grpo/05-rl-prompts/tau.yaml

tau run --config examples/cobol-cpt-grpo/06-grpo/tau.yaml --dry-run=client
tau run --config examples/cobol-cpt-grpo/06-grpo/tau.yaml

tau run --config examples/cobol-cpt-grpo/07-eval/tau.yaml
```

`tau-gated.yaml` is optional. `05-rl-prompts` only needs `01-collect` and
`02-data-prep`, so it can run alongside CPT.

To evaluate the base model or the CPT checkpoint, copy `07-eval/tau.yaml` and
change `MODEL_ID` (`Qwen/Qwen2.5-Coder-32B` or `.../cpt/final`) and `OUT`
(`.../eval/base` or `.../eval/cpt`).

For a short end-to-end check of the GPU stages, set `MAX_STEPS` in
`04-cpt/tau.yaml` or `06-grpo/tau.yaml` (for example `MAX_STEPS: "5"`). A
short CPT run produces a model that is not useful for GRPO.

The tests run on a CPU-only machine with CPU `torch` and a local Ray install:

```bash
cd examples/cobol-cpt-grpo/03-benchmarks && python -m pytest test_build_benchmarks.py
cd examples/cobol-cpt-grpo/05-rl-prompts && python -m pytest test_prepare_rl_prompts.py
cd examples/cobol-cpt-grpo/06-grpo && python -m pytest test_reward.py
cd examples/cobol-cpt-grpo/06-grpo && python -m pytest test_stop_sync.py
cd examples/cobol-cpt-grpo/06-grpo && python -m pytest test_driver.py
cd examples/cobol-cpt-grpo/04-cpt && python -m pytest test_continued_pretrain.py test_driver.py
cd examples/cobol-cpt-grpo/07-eval && python -m pytest test_run_eval.py
cd examples/cobol-cpt-grpo/07-eval && python -m pytest test_driver.py
```

Run `test_reward.py` in its own process; it replaces `torch` and `trl` with
stubs. Run `07-eval/test_driver.py` apart from `test_run_eval.py`; it checks
that the driver process never imports `torch`, which `test_run_eval.py` does.
With GnuCOBOL on `PATH`, `test_run_eval.py` also compiles a reference
completion for a paragraph that performs a later one. `test_stop_sync.py` starts two CPU processes in a gloo group.
`06-grpo/test_driver.py` starts a local Ray cluster and takes one to five
minutes.

## Results

These results predate the paragraph compile fix described under Eval: paragraph
completions were compiled without the rest of the procedure, and no reference
compile counts were recorded. The paragraph and overall compile rows will
change when the eval is rerun.

All three models below were scored by one `07-eval` RayJob each (greedy
decoding, 512 new tokens, `MAX_MODEL_LEN` 32768, TP=8 on 8 H200) on the same
benchmarks. The pipeline ran end to end from this example with
`SOURCE_LICENSES=permissive` and no gated datasets:

- CPT: 60 steps (3 epochs) from `Qwen/Qwen2.5-Coder-32B`. Eval loss was
  0.523 at step 25 and 0.507 at step 60.
- GRPO: 162 steps (3 epochs) from the CPT checkpoint. The compile preflight
  kept 1,762 prompts, 918 free format and 844 fixed format.
- Benchmarks: 62 paragraph, 50 program, 100 structural, and 78 copybook tasks,
  plus 158 held-out programs for perplexity. Three prompts did not fit in the
  context and were set aside for every model: one paragraph task (51,129
  tokens) and two copybook tasks (55,731 and 59,490 tokens). The compile score
  is out of the remaining 211 tasks.

| Metric | Base | CPT | GRPO |
| --- | --- | --- | --- |
| Compile, overall | 50/211 | **89/211** | 88/211 |
| Compile, paragraph completion | 16/61 (6 free + 10 fixed) | 24/61 (9 + 15) | 25/61 (9 + 16) |
| Compile, program completion | 13/50 (8 + 5) | 20/50 (12 + 8) | 20/50 (11 + 9) |
| Compile, structural validation | 21/100 (11 + 10) | 45/100 (23 + 22) | 43/100 (22 + 21) |
| COBOL perplexity | 1.90 | 1.54 | 1.54 |
| BLEU, paragraph / program | 0.130 / 0.056 | 0.176 / 0.096 | 0.180 / 0.099 |
| Edit distance, paragraph / program | 0.677 / 0.661 | 0.632 / 0.632 | 0.632 / 0.623 |
| PERFORM reference accuracy | 15.9% | 40.7% | 34.6% |
| Column formatting | 74.0% | 80.0% | 78.0% |
| PIC clause accuracy | 98.5% | 98.5% | 98.5% |
| Division structure | 100% | 100% | 100% |
| BLEU / edit distance, copybook usage | 0.013 / 0.883 | 0.035 / 0.843 | 0.038 / 0.849 |

CPT accounts for nearly all of the improvement over the base model: compiles
rise from 50 to 89, perplexity falls from 1.90 to 1.54, and PERFORM reference
accuracy more than doubles. GRPO does not improve on CPT on these benchmarks.
Its compile count and BLEU are within one task or a few thousandths of CPT,
and its PERFORM reference accuracy is 6.1 points lower even though the
structural reward includes a PERFORM bonus.
