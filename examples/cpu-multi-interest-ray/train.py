# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import math
from pathlib import Path

import ray
import torch
import torch.nn as nn
import torch.nn.functional as F
import yaml
from ray import train
from ray.train import ScalingConfig
from ray.train.torch import TorchTrainer, prepare_model


NUM_TOPICS = 8
NUM_ITEMS = 1024
ITEMS_PER_TOPIC = NUM_ITEMS // NUM_TOPICS
HISTORY_LEN = 32
EXPLICIT_TOPICS = 2


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
    optimizer = torch.optim.AdamW(model.parameters(), lr=2e-3, weight_decay=1e-3)

    final_loss = 0.0
    final_acc = 0.0
    final_importance = ""
    for step in range(int(config.get("steps", 48))):
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
        if step in (0, 12, 24, 47):
            print(
                f"rank={rank}/{world} step={step} loss={final_loss:.4f} "
                f"pairwise_acc={final_acc:.3f} implicit_cluster_mass=[{final_importance}]",
                flush=True,
            )

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


def parse_args():
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, default=None)
    parser.add_argument("--smoke-pairs", type=int, default=0)
    return parser.parse_args()


def train_workers_from_manifest(path: Path | None) -> int:
    if path is None:
        return 4
    with path.open("r", encoding="utf-8") as f:
        manifest = yaml.safe_load(f) or {}
    compute = manifest.get("compute", {})
    return max(1, int(compute.get("workers", 5)) - 1)


def main():
    args = parse_args()
    num_workers = train_workers_from_manifest(args.manifest)
    steps = 8 if args.smoke_pairs > 0 else 48

    ray.init(address="auto")
    print("cluster_resources", ray.cluster_resources(), flush=True)
    print("torch", torch.__version__, "cuda", torch.cuda.is_available(), flush=True)
    print(
        "demo_source=arxiv:2506.23060v1 "
        "core=implicit DCM + explicit conditional retrieval on synthetic recommendations",
        flush=True,
    )
    trainer = TorchTrainer(
        train_loop_per_worker,
        train_loop_config={"steps": steps},
        scaling_config=ScalingConfig(num_workers=num_workers, use_gpu=False, resources_per_worker={"CPU": 1}),
    )
    result = trainer.fit()
    print("final_metrics", result.metrics, flush=True)


if __name__ == "__main__":
    main()
