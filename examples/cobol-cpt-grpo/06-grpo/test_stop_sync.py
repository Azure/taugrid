# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Local test that a GRPO alarm on one rank stops every rank.

Starts two CPU processes in a gloo group, each running TauGRPOMetricsCallback without
a publisher. Only rank 1 raises the KL alarm; both ranks must set
should_training_stop at the same step. trl, transformers and datasets are replaced
with stubs, so this needs CPU torch only:
  python -m pytest examples/cobol-cpt-grpo/06-grpo/test_stop_sync.py -v
"""

import json
import os
import socket
import subprocess
import sys

RL_DIR = os.path.dirname(os.path.abspath(__file__))

WORKER = """\
import json
import os
import sys
import types

for name in ["datasets", "transformers", "trl"]:
    sys.modules[name] = types.ModuleType(name)
sys.modules["datasets"].Dataset = type("Dataset", (), {})
sys.modules["transformers"].AutoTokenizer = type("AutoTokenizer", (), {})
sys.modules["transformers"].TrainerCallback = type("TrainerCallback", (), {})
sys.modules["trl"].GRPOConfig = type("GRPOConfig", (), {})
sys.modules["trl"].GRPOTrainer = type("GRPOTrainer", (), {})

rl_dir, port, rank, out = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
sys.path.insert(0, rl_dir)

import torch.distributed as dist

import train_grpo

dist.init_process_group(
    "gloo", init_method=f"tcp://127.0.0.1:{port}", rank=rank, world_size=2
)
cb = train_grpo.TauGRPOMetricsCallback()
if rank == 1:
    cb.kl_alarm_count = 8
stops = []
for step in (1, 2):
    control = types.SimpleNamespace(should_training_stop=False)
    kl = 6.0 if rank == 1 else 0.1
    cb.on_log(None, types.SimpleNamespace(global_step=step), control, logs={"kl": kl})
    stops.append(control.should_training_stop)
dist.destroy_process_group()
with open(out, "w") as f:
    json.dump(stops, f)
"""


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_alarm_on_one_rank_stops_both(tmp_path):
    worker = tmp_path / "worker.py"
    worker.write_text(WORKER)
    port = str(_free_port())
    outs = [tmp_path / f"rank-{rank}.json" for rank in (0, 1)]
    procs = [
        subprocess.Popen(
            [sys.executable, str(worker), RL_DIR, port, str(rank), str(outs[rank])]
        )
        for rank in (0, 1)
    ]
    for proc in procs:
        assert proc.wait(timeout=120) == 0
    assert [json.loads(out.read_text()) for out in outs] == [
        [False, True],
        [False, True],
    ]
