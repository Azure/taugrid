# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import ast
import contextlib
import hashlib
import io
import math
import os
from pathlib import Path
import pickle
import shutil
import types
import unittest
from unittest.mock import Mock, patch
import uuid


def workload_functions(filename="fineweb_ray_train.py", selected=None):
    source = Path(__file__).with_name(filename).read_text()
    tree = ast.parse(source)
    if selected is None:
        selected = {"require_rdma_prerequisites", "verify_checkpoint_state",
                    "save_first_checkpoint", "sha256_file"}
    functions = [n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name in selected]
    module = ast.Module(body=[
        ast.ImportFrom(module="__future__", names=[ast.alias(name="annotations")], level=0),
        *functions,
    ], type_ignores=[])
    ns = {"Path": Path, "os": os, "hashlib": hashlib, "math": math}
    exec(compile(ast.fix_missing_locations(module), "<workload functions>", "exec"), ns)
    return ns


class Tensor:
    def __init__(self, data, dtype="float32"):
        self.data, self.dtype = data, dtype


class RuntimeTests(unittest.TestCase):
    def test_inherited_memlock(self):
        for hard, allowed in [(65536, False), (1 << 30, True), (-1, True)]:
            with self.subTest(hard=hard):
                ns = workload_functions()
                resource = Mock(RLIMIT_MEMLOCK=8, RLIM_INFINITY=-1)
                resource.getrlimit.return_value = (65536, hard)
                ns.update(resource=resource, ctypes=Mock(), Path=Mock())
                ns["Path"].return_value.glob.return_value = ["uverbs0"]
                with contextlib.redirect_stdout(io.StringIO()):
                    if allowed:
                        ns["require_rdma_prerequisites"]()
                        resource.setrlimit.assert_called_once_with(8, (hard, hard))
                    else:
                        with self.assertRaisesRegex(RuntimeError, "FINEWEB_RDMA_PREREQUISITE"):
                            ns["require_rdma_prerequisites"]()
                        resource.setrlimit.assert_not_called()

    def test_missing_rdma_library_or_device(self):
        for missing in ("library", "device"):
            with self.subTest(missing=missing):
                ns = workload_functions()
                resource = Mock(RLIMIT_MEMLOCK=8, RLIM_INFINITY=-1)
                resource.getrlimit.return_value = (-1, -1)
                ns.update(resource=resource, ctypes=Mock(), Path=Mock())
                ns["Path"].return_value.glob.return_value = []
                if missing == "library":
                    ns["ctypes"].CDLL.side_effect = OSError("unavailable")
                with self.assertRaisesRegex(RuntimeError, "FINEWEB_RDMA_PREREQUISITE"):
                    ns["require_rdma_prerequisites"]()

    def test_full_state_readback(self):
        root = Path(__file__).resolve().parent / f".runtime-test-{uuid.uuid4().hex}"
        root.mkdir()
        self.addCleanup(shutil.rmtree, root)
        for rank, corrupt in [(0, False), (0, True), (1, False)]:
            with self.subTest(rank=rank, corrupt=corrupt):
                ns = workload_functions()
                fsdp = Mock()
                fsdp.state_dict_type.return_value = contextlib.nullcontext()
                fsdp.optim_state_dict.return_value = {
                    "state": {"weight": {"exp_avg": Tensor([3.0])}},
                    "param_groups": [{"lr": 0.001}],
                }
                model = Mock()
                model.state_dict.return_value = {"weight": Tensor([1.0, 2.0])}
                load_calls = []

                def save(state, path):
                    Path(path).write_bytes(pickle.dumps(state))

                def load(path, **kwargs):
                    load_calls.append(kwargs)
                    state = pickle.loads(Path(path).read_bytes())
                    if corrupt:
                        state["optimizer"]["state"]["weight"]["exp_avg"].data = [9.0]
                    return state

                ns.update(
                    torch=types.SimpleNamespace(
                        Tensor=Tensor, equal=lambda a, b: a.data == b.data, save=save, load=load,
                    ),
                    FSDP=fsdp, FullStateDictConfig=Mock(), FullOptimStateDictConfig=Mock(),
                    StateDictType=Mock(), dist=Mock(),
                )
                output = io.StringIO()
                checkpoint_dir = root / f"{rank}-{corrupt}"
                with patch.dict(os.environ, {"FINEWEB_WORKER_POD": "rank-zero"}), contextlib.redirect_stdout(output):
                    if corrupt:
                        with self.assertRaisesRegex(RuntimeError, "readback mismatch"):
                            ns["save_first_checkpoint"](model, "optimizer", checkpoint_dir, 50, 1716000000, rank)
                        self.assertNotIn("FINEWEB_FIRST_CHECKPOINT", output.getvalue())
                        self.assertFalse((checkpoint_dir / "fineweb-step-50.pt").exists())
                        ns["dist"].barrier.assert_not_called()
                    else:
                        ns["save_first_checkpoint"](model, "optimizer", checkpoint_dir, 50, 1716000000, rank)
                        ns["dist"].barrier.assert_called_once()
                        if rank == 0:
                            path = checkpoint_dir / "fineweb-step-50.pt"
                            self.assertIn("readback=verified", output.getvalue())
                            self.assertIn(hashlib.sha256(path.read_bytes()).hexdigest(), output.getvalue())
                            self.assertIn("optimizer", pickle.loads(path.read_bytes()))
                        else:
                            self.assertFalse(checkpoint_dir.exists())
                            self.assertNotIn("FINEWEB_FIRST_CHECKPOINT", output.getvalue())
                expected_calls = [] if rank else [{"map_location": "cpu", "weights_only": True, "mmap": True}]
                self.assertEqual(load_calls, expected_calls)
                fsdp.optim_state_dict.assert_called_once_with(model, "optimizer")

    def test_readback_rejects_structural_corruption(self):
        ns = workload_functions()
        ns["torch"] = types.SimpleNamespace(Tensor=Tensor, equal=lambda a, b: a.data == b.data)
        for expected, actual in [
            ({"x": 1}, {"y": 1}), ([1], (1,)), ([1], []), (1, True),
            (Tensor([1]), Tensor([1], "float16")), (Tensor([1]), [1]),
        ]:
            with self.subTest(expected=expected, actual=actual):
                with self.assertRaisesRegex(RuntimeError, "readback mismatch"):
                    ns["verify_checkpoint_state"](expected, actual)

    def test_nanogpt_sizing_and_zero_cpu_coordinator(self):
        for env, dimensions in [({}, (4, 4, 256)), (
            {"NANOGPT_N_LAYER": "8", "NANOGPT_N_HEAD": "8", "NANOGPT_N_EMBD": "512"},
            (8, 8, 512),
        )]:
            with self.subTest(env=env):
                ns = workload_functions("nanogpt_ray_train.py", {"main", "env_int", "final_metrics_from_result"})
                scaling = Mock()
                trainer = Mock()
                trainer.return_value.fit.return_value.metrics = {"step": 2000, "world_size": 16, "loss": 1.0}
                ray = Mock()
                ray.cluster_resources.return_value = {"GPU": 16}
                ns.update(ray=ray, TorchTrainer=trainer, ScalingConfig=scaling, RunConfig=Mock(),
                          train_loop=Mock(), parse_dataset_contract=lambda: (["shard"], ["digest"], [100]))
                with patch.dict(os.environ, env, clear=True), contextlib.redirect_stdout(io.StringIO()):
                    ns["main"]()
                config = trainer.call_args.kwargs["train_loop_config"]
                self.assertEqual(tuple(config[k] for k in ("n_layer", "n_head", "n_embd")), dimensions)
                scaling.assert_called_once_with(num_workers=16, use_gpu=True, trainer_resources={"CPU": 0})


if __name__ == "__main__":
    unittest.main()
