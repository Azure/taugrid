"""Distributed Ray Train image-ResNet sample for Tau/Stellar rich artifacts.

The sample intentionally keeps model semantics in the training job. Tau/Stellar
only sees generic artifacts: images, an HTML report, JSON tables, and checkpoint
metadata. It uses synthetic image data by default so the E2E does not depend on
an external dataset service.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path
from typing import Any

import ray
import torch
import torch.nn as nn
from ray import train
from ray.train import RunConfig, ScalingConfig
from ray.train import torch as ray_train_torch
from ray.train.torch import TorchTrainer
from tau import stellar


NUM_CLASSES = int(os.environ.get("IMAGERESNET_CLASSES", "10"))
IMAGE_SIZE = int(os.environ.get("IMAGERESNET_IMAGE_SIZE", "64"))
ARTIFACT_CLASSES = min(NUM_CLASSES, int(os.environ.get("IMAGERESNET_ARTIFACT_CLASSES", "10")))


class BasicBlock(nn.Module):
    def __init__(self, channels: int) -> None:
        super().__init__()
        self.net = nn.Sequential(
            nn.Conv2d(channels, channels, kernel_size=3, padding=1, bias=False),
            nn.BatchNorm2d(channels),
            nn.ReLU(inplace=True),
            nn.Conv2d(channels, channels, kernel_size=3, padding=1, bias=False),
            nn.BatchNorm2d(channels),
        )
        self.relu = nn.ReLU(inplace=True)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.relu(x + self.net(x))


class TinyResNet(nn.Module):
    def __init__(self, num_classes: int) -> None:
        super().__init__()
        self.stem = nn.Sequential(
            nn.Conv2d(3, 32, kernel_size=3, padding=1, bias=False),
            nn.BatchNorm2d(32),
            nn.ReLU(inplace=True),
        )
        self.blocks = nn.Sequential(BasicBlock(32), BasicBlock(32), BasicBlock(32))
        self.pool = nn.AdaptiveAvgPool2d((1, 1))
        self.head = nn.Linear(32, num_classes)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        x = self.stem(x)
        x = self.blocks(x)
        x = self.pool(x).flatten(1)
        return self.head(x)


def env_int(name: str, default: int) -> int:
    raw = os.environ.get(name, "")
    if raw == "":
        return default
    return int(raw)


def make_batch(step: int, batch_size: int, device: torch.device, rank: int) -> tuple[torch.Tensor, torch.Tensor]:
    generator = torch.Generator(device="cpu").manual_seed(17_000 + rank * 997 + step)
    labels = torch.arange(batch_size, dtype=torch.long) % NUM_CLASSES
    labels = torch.roll(labels, shifts=step % max(NUM_CLASSES, 1))
    images = torch.rand((batch_size, 3, IMAGE_SIZE, IMAGE_SIZE), generator=generator)
    class_signal = labels.float().view(batch_size, 1, 1, 1) / max(NUM_CLASSES - 1, 1)
    images[:, 0:1, :, :] = (images[:, 0:1, :, :] * 0.35) + (class_signal * 0.65)
    return images.to(device), labels.to(device)


def write_json(path: Path, value: Any) -> int:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return time.time_ns()


def write_text(path: Path, value: str) -> int:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(value, encoding="utf-8")
    return time.time_ns()


def write_png(path: Path, tensor: torch.Tensor, title: str | None = None) -> int:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    path.parent.mkdir(parents=True, exist_ok=True)
    image = tensor.detach().cpu().clamp(0, 1).permute(1, 2, 0).numpy()
    plt.figure(figsize=(3, 3))
    plt.imshow(image)
    if title:
        plt.title(title)
    plt.axis("off")
    plt.tight_layout()
    plt.savefig(path)
    plt.close()
    return time.time_ns()


def write_confusion_matrix(path: Path, matrix: torch.Tensor) -> int:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    path.parent.mkdir(parents=True, exist_ok=True)
    plt.figure(figsize=(7, 6))
    plt.imshow(matrix.cpu().numpy(), cmap="viridis")
    plt.title("Synthetic image-ResNet confusion matrix")
    plt.xlabel("Predicted")
    plt.ylabel("Actual")
    plt.colorbar()
    plt.tight_layout()
    plt.savefig(path)
    plt.close()
    return time.time_ns()


def train_loop(config: dict[str, Any]) -> None:
    run_id = config["run_id"]
    rank = train.get_context().get_world_rank()
    world_size = train.get_context().get_world_size()
    device = ray_train_torch.get_device()
    steps = int(config["steps"])
    batch_size = int(config["batch_size"])

    model = ray_train_torch.prepare_model(TinyResNet(NUM_CLASSES).to(device))
    optimizer = torch.optim.AdamW(model.parameters(), lr=float(config["lr"]))
    loss_fn = nn.CrossEntropyLoss()

    for step in range(1, steps + 1):
        images, labels = make_batch(step, batch_size, device, rank)
        logits = model(images)
        loss = loss_fn(logits, labels)
        optimizer.zero_grad(set_to_none=True)
        loss.backward()
        optimizer.step()
        train.report({"train/loss": float(loss.detach().cpu()), "step": step})

    if rank != 0:
        return

    artifact_root = Path(config["artifact_root"])
    checkpoint_root = Path(config["checkpoint_root"])
    artifact_root.mkdir(parents=True, exist_ok=True)
    checkpoint_root.mkdir(parents=True, exist_ok=True)

    eval_images, eval_labels = make_batch(steps + 1, batch_size * 2, device, rank)
    with torch.no_grad():
        logits = model(eval_images)
        predictions = logits.argmax(dim=1)
    matrix = torch.zeros((NUM_CLASSES, NUM_CLASSES), dtype=torch.int64)
    for actual, predicted in zip(eval_labels.cpu(), predictions.cpu(), strict=False):
        matrix[int(actual), int(predicted)] += 1
    per_class = []
    for class_id in range(NUM_CLASSES):
        total = int(matrix[class_id].sum().item())
        correct = int(matrix[class_id, class_id].item())
        per_class.append({
            "class_id": class_id,
            "class_name": f"synthetic-{class_id}",
            "top1_accuracy": (correct / total) if total else 0.0,
            "samples": total,
        })

    close_times: list[dict[str, Any]] = []
    run = stellar.init(
        project=config["project"],
        run=run_id,
        group=config["group"],
        experiment=config["experiment"],
        experiment_id=config["experiment_id"],
        dir=config["stellar_dir"],
        store=config["expstore"],
        owner="imageresnet-ray-train",
        tau_binary=config["tau_binary"],
        config={
            "model": "tiny-resnet",
            "world_size": world_size,
            "num_classes": NUM_CLASSES,
            "steps": steps,
            "batch_size": batch_size,
        },
    )
    run.log({"train/loss": float(loss.detach().cpu()), "val/top1": sum(row["top1_accuracy"] for row in per_class) / len(per_class)}, step=steps)

    confusion_path = artifact_root / "confusion_matrix.png"
    close_times.append({"name": "confusion_matrix", "closed_unix_ns": write_confusion_matrix(confusion_path, matrix)})
    logged = run.log_artifact("confusion_matrix", stellar.Image(confusion_path), artifact_type="image")
    close_times[-1]["logged_path"] = str(logged)

    for class_id in range(ARTIFACT_CLASSES):
        sample_path = artifact_root / f"class_samples_{class_id}.png"
        image = eval_images[class_id % eval_images.size(0)]
        close_times.append({"name": f"class_samples_{class_id}", "closed_unix_ns": write_png(sample_path, image, f"class {class_id}")})
        logged = run.log_artifact(f"class_samples_{class_id}", stellar.Image(sample_path), artifact_type="image")
        close_times[-1]["logged_path"] = str(logged)

    for offset, class_id in enumerate(range(ARTIFACT_CLASSES)):
        sample_path = artifact_root / f"worst_class_samples_{class_id}.png"
        image = eval_images[(offset + ARTIFACT_CLASSES) % eval_images.size(0)]
        close_times.append({"name": f"worst_class_samples_{class_id}", "closed_unix_ns": write_png(sample_path, image, f"worst class {class_id}")})
        logged = run.log_artifact(f"worst_class_samples_{class_id}", stellar.Image(sample_path), artifact_type="image")
        close_times[-1]["logged_path"] = str(logged)

    accuracy_path = artifact_root / "per_class_accuracy.json"
    close_times.append({"name": "per_class_accuracy", "closed_unix_ns": write_json(accuracy_path, per_class)})
    logged = run.log_artifact("per_class_accuracy", stellar.Table(per_class), artifact_type="table")
    close_times[-1]["logged_path"] = str(logged)

    report_html = """
<!doctype html>
<html>
  <head><meta charset="utf-8"><title>image-ResNet validation report</title></head>
  <body>
    <h1>image-ResNet validation report</h1>
    <p>Run {run_id} trained on {world_size} Ray workers and emitted generic Tau/Stellar artifacts.</p>
    <img src="confusion_matrix.png" alt="confusion matrix" style="max-width: 640px">
    <p>Top-1 mean accuracy: {top1:.4f}</p>
  </body>
</html>
""".format(run_id=run_id, world_size=world_size, top1=sum(row["top1_accuracy"] for row in per_class) / len(per_class))
    report_path = artifact_root / "val_report.html"
    close_times.append({"name": "val_report", "closed_unix_ns": write_text(report_path, report_html)})
    logged = run.log_artifact("val_report", stellar.Html(report_html), artifact_type="html")
    close_times[-1]["logged_path"] = str(logged)

    checkpoint_path = checkpoint_root / f"{run_id}-epoch-final.pt"
    checkpoint_path.parent.mkdir(parents=True, exist_ok=True)
    torch.save({"run_id": run_id, "state_dict": model.module.state_dict() if hasattr(model, "module") else model.state_dict()}, checkpoint_path)
    checkpoint_meta = [{
        "step": steps,
        "epoch": 1,
        "path": str(checkpoint_path),
        "bytes": checkpoint_path.stat().st_size,
        "rank": 0,
    }]
    checkpoint_meta_path = artifact_root / "checkpoint_meta.json"
    close_times.append({"name": "checkpoint_meta", "closed_unix_ns": write_json(checkpoint_meta_path, checkpoint_meta)})
    logged = run.log_artifact("checkpoint_meta", stellar.Table(checkpoint_meta), artifact_type="table")
    close_times[-1]["logged_path"] = str(logged)

    close_times_path = artifact_root / "artifact_close_times.json"
    close_times.append({"name": "artifact_close_times", "closed_unix_ns": write_json(close_times_path, close_times)})
    logged = run.log_artifact("artifact_close_times", stellar.Table(close_times), artifact_type="table")
    close_times[-1]["logged_path"] = str(logged)

    run.finish(sync=True)
    completion_file = config.get("completion_file")
    if completion_file:
        Path(completion_file).write_text("done\n", encoding="utf-8")
    print(
        "IMAGERESNET_ARTIFACTS_WRITTEN "
        f"run={run_id} artifacts={len(close_times)} expstore={config['expstore']} "
        f"artifact_root={artifact_root} checkpoint={checkpoint_path}"
    )


def main() -> None:
    run_id = os.environ.get("IMAGERESNET_RUN_ID", f"imageresnet-{int(time.time())}")
    data_root = Path(os.environ.get("IMAGERESNET_DATA_ROOT", "/data"))
    num_workers = env_int("IMAGERESNET_NUM_WORKERS", 2)
    config = {
        "run_id": run_id,
        "project": os.environ.get("STELLAR_PROJECT", "imageresnet"),
        "group": os.environ.get("STELLAR_GROUP", "h200-rich-artifacts"),
        "experiment": os.environ.get("STELLAR_EXPERIMENT", "Can Tau/Stellar render distributed image-ResNet rich artifacts?"),
        "experiment_id": os.environ.get("STELLAR_EXPERIMENT_ID", "imageresnet-rich-artifacts"),
        "expstore": os.environ.get("TAU_EXP_STORE", str(data_root / "tau-exp")),
        "stellar_dir": os.environ.get("TAU_STELLAR_DIR", str(data_root / "tau-stellar")),
        "artifact_root": os.environ.get("IMAGERESNET_ARTIFACT_ROOT", str(data_root / "artifacts" / run_id)),
        "checkpoint_root": os.environ.get("IMAGERESNET_CHECKPOINT_ROOT", str(data_root / "checkpoints" / run_id)),
        "completion_file": os.environ.get("IMAGERESNET_COMPLETION_FILE", str(data_root / "tau-artifacts.done")),
        "tau_binary": os.environ.get("TAU_BINARY", "tau"),
        "steps": env_int("IMAGERESNET_STEPS", 8),
        "batch_size": env_int("IMAGERESNET_BATCH_SIZE", 32),
        "lr": float(os.environ.get("IMAGERESNET_LR", "0.001")),
    }
    ray.init(address=os.environ.get("RAY_ADDRESS", "auto"))
    trainer = TorchTrainer(
        train_loop_per_worker=train_loop,
        train_loop_config=config,
        scaling_config=ScalingConfig(num_workers=num_workers, use_gpu=True, resources_per_worker={"GPU": 1}),
        run_config=RunConfig(name=run_id, storage_path=os.environ.get("RAY_AIR_STORAGE_PATH", str(data_root / "ray-results"))),
    )
    trainer.fit()


if __name__ == "__main__":
    main()
