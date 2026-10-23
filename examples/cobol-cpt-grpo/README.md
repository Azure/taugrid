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
retried with `-fixed` if that fails, and both counts are reported. The headline
compile score is out of 220 tasks: 70 paragraph, 50 program, and 100
structural. Before loading the model, the eval checks that `cobc --version`
works and that all five suite files exist and are not empty. It fails if
either check fails, and it fails if `cobc` disappears during the run. It also
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
cd examples/cobol-cpt-grpo/07-eval && python -m pytest test_run_eval.py test_driver.py
```

Run `test_reward.py` in its own process; it replaces `torch` and `trl` with
stubs. `test_stop_sync.py` starts two CPU processes in a gloo group.
`06-grpo/test_driver.py` starts a local Ray cluster and takes one to five
minutes.

## Results

The numbers in this section come from an earlier version of the pipeline that
had two defects, both fixed in the code here:

- Stage 05 did not exclude the validation split, so GRPO prompts included
  validation programs. For up to 18/50 program-completion, 5/70 paragraph, and
  19/100 structural tasks, the reference answer appears in a GRPO training
  prompt. The reward only checks compilation, so GRPO never trained on those
  answers. CPT is not affected.
- GRPO compiled every prompt with `cobc -free`, so it dropped the 3,676 of
  6,852 prompts (54%) that compile only in fixed format.

The GRPO results and the GRPO gains below are therefore partly contaminated,
and they come from half of the intended prompts. The CPT columns are not
affected. Copybook usage was not scored at the time. GRPO and eval have not
yet been rerun with the fixes.

The GRPO model was evaluated with `07-eval` (greedy decoding, 512 new tokens,
TP=8 on 8 H200):

| Metric | GRPO model |
| --- | --- |
| Compile, overall | **64/220** |
| Compile, paragraph completion | 18/70 (7 free + 11 fixed) |
| Compile, program completion | 18/50 |
| Compile, structural validation | 28/100 (16 free + 12 fixed) |
| COBOL perplexity | 1.49 |
| BLEU, paragraph / program | 0.155 / 0.190 |
| PERFORM reference accuracy | 41.8% |
| Column formatting | 72.0% |
| PIC clause accuracy | 98.9% |

The checkpoint was trained with an earlier Job-based launch of the same CPT
and GRPO code. Running the `07-eval` RayJob on that checkpoint reproduced
every number above exactly.

A full rerun with every stage launched from this example scored lower. The
Job-based CPT column is the checkpoint that the GRPO model above started from.


| Metric | Job-based CPT | RayJob CPT | RayJob GRPO |
| --- | --- | --- | --- |
| Compile, overall | 58/220 | 53/220 | 57/220 |
| Compile, paragraph completion | 16/70 | 14/70 | 14/70 |
| Compile, program completion | 16/50 | 18/50 | 18/50 |
| Compile, structural validation | 26/100 | 21/100 | 25/100 |
| COBOL perplexity | 1.49 | 1.48 | 1.48 |
| PERFORM reference accuracy | 40.4% | 27.3% | 28.1% |

All three columns were scored with the same `07-eval` RayJob. GRPO adds a
similar amount on either CPT checkpoint (+4 on the RayJob CPT, +6 on the
Job-based CPT), so the gap comes from CPT. The cause is not yet known. The
training image moved to newer `transformers` and DeepSpeed releases than the
Job-based run used.
