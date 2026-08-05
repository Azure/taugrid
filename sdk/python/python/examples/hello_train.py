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
    # In a real run you'd torch.load() / train your model here.
    # Smoke runs early-exit; full runs train to completion.


if __name__ == "__main__":
    # Local run: uses cwd-rooted paths, no cluster involved.
    hello()

    # Remote run: shells to the Tau Go CLI behind the scenes. Build the binary first:
    #   cd cli && go build -o /usr/local/bin/tau ./cmd/tau
    #
    # hello.submit(dry_run="client")     # render only, don't apply
    # hello.submit()                      # apply for real
