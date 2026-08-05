"""CPU PyTorch training with real Tau Stellar SDK experiment logging.

Same multi-interest retrieval model as ../../cpu-multi-interest-ray/train.py
(arxiv:2506.23060v1 -- implicit DCM + explicit conditional retrieval), but:
  * a single Ray Train worker (workers: 2 in tau.yaml == 1 head + 1 worker
    pod), so there is exactly one pod to `kubectl cp` Stellar output from
  * every step calls `tau.stellar`'s real Run.log()/finish() -- the same
    SDK a researcher's own project would import -- writing local files
    under /tmp/tau_stellar_demo inside the pod (see README.md for how those
    files are retrieved and viewed after the run completes)

`from tau import stellar` resolves against the `tau/` package shipped
alongside this script via `run.working_dir` (see tau.yaml in this
directory) -- a verbatim copy of sdk/python/tau/stellar.py, since the Tau
Python SDK has no published wheel to `pip install` inside the workload
image. stellar.py has no dependency on the rest of the SDK at import time
(see tau/__init__.py).
"""

import math
import os
import time

import ray
import torch
import torch.nn as nn
import torch.nn.functional as F
from ray import train
from ray.train import ScalingConfig
from ray.train.torch import TorchTrainer, prepare_model

from tau import stellar

NUM_TOPICS = 8
NUM_ITEMS = 1024
ITEMS_PER_TOPIC = NUM_ITEMS // NUM_TOPICS
HISTORY_LEN = 32
EXPLICIT_TOPICS = 2

STELLAR_DIR = "/tmp/tau_stellar_demo"


class DifferentiableClusteringModule(nn.Module):
    """Tiny DCM: learn interest queries that softly cluster a user's history."""

    def __init__(self, dim: int, num_interests: int):
        super().__init__()
        self.queries = nn.Parameter(torch.randn(num_interests, dim) * 0.02)
        self.proj = nn.Linear(dim, dim)

    def forward(self, history_embeddings: torch.Tensor):
        logits = torch.einsum("kd,bld->bkl", self.queries, self.proj(history_embeddings))
        logits = logits / math.sqrt(history_embeddings.size(-1))
        weights = logits.softmax(dim=-1)
        interests = torch.einsum("bkl,bld->bkd", weights, history_embeddings)
        return F.normalize(interests, dim=-1), weights


class MultiInterestRetrievalModel(nn.Module):
    def __init__(self, dim: int = 256, num_implicit_interests: int = 4):
        super().__init__()
        self.item_id = nn.Embedding(NUM_ITEMS, dim)
        self.topic = nn.Embedding(NUM_TOPICS, dim)
        self.item_encoder = nn.Sequential(nn.Linear(dim * 2, dim), nn.ReLU(), nn.Linear(dim, dim))
        self.dcm = DifferentiableClusteringModule(dim, num_implicit_interests)
        self.explicit_cross = nn.Sequential(nn.Linear(dim * 3, dim), nn.ReLU(), nn.Linear(dim, dim))

    def encode_item(self, item_ids: torch.Tensor, item_topics: torch.Tensor):
        x = torch.cat([self.item_id(item_ids), self.topic(item_topics)], dim=-1)
        return F.normalize(self.item_encoder(x), dim=-1)

    def user_interests(self, history_items: torch.Tensor, followed_topics: torch.Tensor, item_topics: torch.Tensor):
        history_topics = item_topics[history_items]
        history = self.encode_item(history_items, history_topics)
        implicit, weights = self.dcm(history)

        profile = history.mean(dim=1)
        followed = self.topic(followed_topics)
        profile_expanded = profile.unsqueeze(1).expand(-1, followed.size(1), -1)
        explicit = self.explicit_cross(torch.cat([profile_expanded, followed, profile_expanded * followed], dim=-1))
        explicit = F.normalize(explicit, dim=-1)
        return torch.cat([implicit, explicit], dim=1), weights

    def score(self, history_items, followed_topics, target_items, target_topics, item_topics):
        interests, weights = self.user_interests(history_items, followed_topics, item_topics)
        target = self.encode_item(target_items, target_topics)
        all_scores = torch.einsum("bkd,bd->bk", interests, target) * math.sqrt(target.size(-1))
        return all_scores.max(dim=1).values, weights

    def forward(self, history_items, followed_topics, target_items, target_topics, item_topics):
        return self.score(history_items, followed_topics, target_items, target_topics, item_topics)


def items_for_topics(topic_ids: torch.Tensor, generator: torch.Generator):
    slots = torch.randint(0, ITEMS_PER_TOPIC, topic_ids.shape, generator=generator)
    return topic_ids + NUM_TOPICS * slots


def sample_batch(batch_size: int, rank: int, step: int):
    generator = torch.Generator().manual_seed(10_000 + rank * 1_000 + step)
    item_topics = torch.arange(NUM_ITEMS) % NUM_TOPICS

    primary = torch.randint(0, NUM_TOPICS, (batch_size,), generator=generator)
    secondary = (primary + torch.randint(1, NUM_TOPICS, (batch_size,), generator=generator)) % NUM_TOPICS
    followed = torch.stack([primary, secondary], dim=1)

    choose_primary = torch.rand((batch_size, HISTORY_LEN), generator=generator) < 0.65
    history_topics = torch.where(choose_primary, primary[:, None], secondary[:, None])
    history_items = items_for_topics(history_topics, generator)

    choose_positive_primary = torch.rand((batch_size,), generator=generator) < 0.55
    positive_topics = torch.where(choose_positive_primary, primary, secondary)
    positive_items = items_for_topics(positive_topics, generator)

    negative_offsets = torch.randint(2, NUM_TOPICS, (batch_size,), generator=generator)
    negative_topics = (primary + negative_offsets) % NUM_TOPICS
    negative_items = items_for_topics(negative_topics, generator)
    return history_items, followed, positive_items, positive_topics, negative_items, negative_topics, item_topics


def train_loop_per_worker(config):
    ctx = train.get_context()
    rank = ctx.get_world_rank()
    world = ctx.get_world_size()
    torch.manual_seed(2026 + rank)

    model = MultiInterestRetrievalModel()
    param_count = sum(p.numel() for p in model.parameters())
    model = prepare_model(model)
    lr = float(config.get("lr", 2e-3))
    weight_decay = float(config.get("weight_decay", 1e-3))
    optimizer = torch.optim.AdamW(model.parameters(), lr=lr, weight_decay=weight_decay)

    steps = int(config.get("steps", 48))
    # Optional wall-clock bound. A CPU step here costs well under a second, so a
    # step-count-only run finishes long before an operator can open the Ray or
    # Kueue dashboards and see it live. Setting max_seconds > 0 keeps real
    # training running for a predictable duration regardless of node speed, which
    # is what makes the live-dashboard walkthrough reproducible. 0 disables it and
    # restores pure step-count behaviour.
    max_seconds = float(config.get("max_seconds", 0) or 0)
    # How often to print a progress line. The default only prints at 4 steps,
    # which is fine for a 48-step run but leaves a long run looking hung when you
    # are watching `tau run logs -f`.
    log_every = int(config.get("log_every", 0) or 0)
    # `question` is what Stellar surfaces as an *experiment* in the dashboard,
    # so a sweep that varies it produces several selectable experiments rather
    # than several runs inside one. `variant` just labels the run.
    experiment = str(config.get("experiment") or "") or None
    variant = str(config.get("variant") or "") or "baseline"

    # Real Stellar SDK run: local-only (sync=False, the default). Written to
    # /tmp/tau_stellar_demo inside this pod's ephemeral filesystem; retrieved
    # after the run via `kubectl cp` per README.md. rank==0 is the only rank
    # here (num_workers=1), so there is no cross-rank coordination to do.
    run = None
    if rank == 0:
        run = stellar.init(
            project="taugrid-cpu-quickstart",
            name=f"{variant}-{int(time.time())}",
            group="stellar-demo",
            # Stellar's dashboard hierarchy is project -> experiment -> run (the
            # older "question" axis was folded into experiment by the expstore
            # v1->v2 migration). Each distinct DEMO_EXPERIMENT value becomes its
            # own selectable experiment in the dashboard; experiment_id is
            # derived from this name.
            experiment=experiment,
            dir=STELLAR_DIR,
            config={
                "steps": steps,
                "max_seconds": max_seconds,
                "lr": lr,
                "weight_decay": weight_decay,
                "variant": variant,
                "model_parameters": param_count,
            },
        )
        print(f"stellar_run_dir={run.dir}", flush=True)

    final_loss = 0.0
    final_acc = 0.0
    final_importance = ""
    loop_start = time.time()
    completed_steps = 0
    for step in range(steps):
        batch = sample_batch(batch_size=128, rank=rank, step=step)
        history, followed, pos_items, pos_topics, neg_items, neg_topics, item_topics = batch
        pos_scores, weights = model(history, followed, pos_items, pos_topics, item_topics)
        neg_scores, _ = model(history, followed, neg_items, neg_topics, item_topics)
        logits = torch.cat([pos_scores, neg_scores], dim=0)
        labels = torch.cat([torch.ones_like(pos_scores), torch.zeros_like(neg_scores)], dim=0)
        loss = F.binary_cross_entropy_with_logits(logits, labels)

        optimizer.zero_grad(set_to_none=True)
        loss.backward()
        optimizer.step()

        final_loss = float(loss.item())
        final_acc = float((pos_scores > neg_scores).float().mean().item())
        importance = weights[0].sum(dim=-1).detach().tolist()
        final_importance = ",".join(f"{x:.2f}" for x in importance)

        if run is not None:
            run.log(
                {
                    "train/loss": final_loss,
                    "train/pairwise_accuracy": final_acc,
                },
                step=step,
            )

        completed_steps = step + 1
        elapsed = time.time() - loop_start
        should_log = step in (0, 12, 24, steps - 1)
        if log_every > 0 and step % log_every == 0:
            should_log = True
        if should_log:
            print(
                f"rank={rank}/{world} step={step} loss={final_loss:.4f} "
                f"pairwise_acc={final_acc:.3f} elapsed={elapsed:.0f}s "
                f"implicit_cluster_mass=[{final_importance}]",
                flush=True,
            )
        if max_seconds > 0 and elapsed >= max_seconds:
            print(
                f"rank={rank}/{world} reached max_seconds={max_seconds:.0f}s after "
                f"{completed_steps} steps; stopping training loop",
                flush=True,
            )
            break

    sample_summary = ""
    if rank == 0:
        raw = model.module if hasattr(model, "module") else model
        raw.eval()
        history, followed, _, _, _, _, item_topics = sample_batch(batch_size=1, rank=rank, step=999)
        catalog_items = torch.arange(NUM_ITEMS)
        catalog_topics = item_topics[catalog_items]
        repeated_history = history.repeat(NUM_ITEMS, 1)
        repeated_followed = followed.repeat(NUM_ITEMS, 1)
        with torch.no_grad():
            scores, weights = raw.score(repeated_history, repeated_followed, catalog_items, catalog_topics, item_topics)
            top_scores, top_items = torch.topk(scores, k=8)
        top_pairs = [
            f"item={int(item)} topic={int(item_topics[item])} score={float(score):.3f}"
            for item, score in zip(top_items, top_scores)
        ]
        sample_summary = "; ".join(top_pairs)
        print("paper_demo=multi_embedding_retrieval_with_dcm_and_conditional_topics", flush=True)
        print(f"followed_topics={followed[0].tolist()} top_recommendations={sample_summary}", flush=True)
        print(f"model_parameters={param_count}", flush=True)

        if run is not None:
            run.log_artifact("top_recommendations", stellar.Table(
                rows=[
                    {"item": int(item), "topic": int(item_topics[item]), "score": float(score)}
                    for item, score in zip(top_items, top_scores)
                ],
                columns=["item", "topic", "score"],
                caption="Top-8 catalog items retrieved for a sampled synthetic user profile",
            ))
            run.finish()
            print(f"stellar_run_finished dir={run.dir}", flush=True)

    train.report(
        {
            "loss": final_loss,
            "pairwise_accuracy": final_acc,
            "rank": rank,
            "world_size": world,
            "parameters": param_count,
            "implicit_cluster_mass": final_importance,
            "sample_recommendations": sample_summary,
        }
    )


def _env_float(key: str, default: float) -> float:
    raw = os.environ.get(key)
    return default if raw is None or raw == "" else float(raw)


def _env_int(key: str, default: int) -> int:
    raw = os.environ.get(key)
    return default if raw is None or raw == "" else int(raw)


def main():
    # This demo runs under the direct `tau run --config` schema (engine: ray),
    # which invokes the entrypoint as `python3 -m train` with no CLI args and
    # no embedded manifest file -- unlike the managed-workflow schema used by
    # the sibling ../tau.yaml demo. tau.yaml's compute.workers: 2 (1 head + 1
    # worker pod) is mirrored here directly: a single Ray Train worker process
    # keeps this demo's `kubectl cp` retrieval step unambiguous (see
    # README.md) -- there is exactly one worker pod to find.
    num_workers = 1
    steps = _env_int("DEMO_STEPS", 48)
    # Wall-clock bound and log cadence for the live-dashboard walkthrough. Both
    # default to off, so existing sweep configs behave exactly as before.
    max_seconds = _env_float("DEMO_MAX_SECONDS", 0.0)
    log_every = _env_int("DEMO_LOG_EVERY", 0)

    ray.init(address="auto")
    print("cluster_resources", ray.cluster_resources(), flush=True)
    print("torch", torch.__version__, "cuda", torch.cuda.is_available(), flush=True)
    print(
        "demo_source=arxiv:2506.23060v1 "
        "core=implicit DCM + explicit conditional retrieval on synthetic recommendations "
        "+ tau.stellar experiment logging",
        flush=True,
    )
    trainer = TorchTrainer(
        train_loop_per_worker,
        train_loop_config={
            "steps": steps,
            "max_seconds": max_seconds,
            "log_every": log_every,
            # Hyperparameters are read from the environment so a sweep can be
            # driven entirely from `runtime.env` in tau.yaml -- no code edit
            # and no rebuild per variant. See README.md > Running a sweep.
            "lr": _env_float("DEMO_LR", 2e-3),
            "weight_decay": _env_float("DEMO_WEIGHT_DECAY", 1e-3),
            "experiment": os.environ.get("DEMO_EXPERIMENT", ""),
            "variant": os.environ.get("DEMO_VARIANT", ""),
        },
        scaling_config=ScalingConfig(num_workers=num_workers, use_gpu=False, resources_per_worker={"CPU": 1}),
    )
    result = trainer.fit()
    print("final_metrics", result.metrics, flush=True)


if __name__ == "__main__":
    main()
