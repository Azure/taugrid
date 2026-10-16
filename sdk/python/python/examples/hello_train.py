# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Minimal hello-world for tau-py.

Local: `python hello_train.py` runs `hello(ctx)` with cwd-rooted paths.
Remote: uncomment the `.submit()` call to ship to the tau cluster.

No YAML, no kubectl, no Helm. The decorator is the manifest.
"""

import tau

@tau.train(
    name="hello-py",
    gpus=1,
    smoke_pairs=4,
    team="research",
    extra_manifest={"runtime": {"pip": ["torch==2.4.0", "transformers"]}},
)
def hello(ctx):
    print(f"hello from {ctx.name}: {ctx.gpus} GPUs")
    print(f"  remote?      {ctx.is_remote}")
    print(f"  datasets:    {ctx.datasets_dir}")
    print(f"  checkpoints: {ctx.checkpoints_dir}")
    print(f"  smoke_pairs: {ctx.smoke_pairs}")

if __name__ == "__main__":
    hello()
