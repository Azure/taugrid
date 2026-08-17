# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Minimal local package so `train.py` can do `from tau import stellar` using
# the real, unmodified `sdk/python/tau/stellar.py` SDK module (a byte-identical
# copy) without pulling in the rest of the Tau Python SDK (`workloads.py`,
# `_cluster.py`, etc.), which pull in dependencies this CPU demo image does not
# install.
#
# This package is shipped to the Ray worker pod via `run.working_dir` (Ray
# runtime_env project archive), not installed from PyPI: the Tau Python SDK
# has no published wheel today (see sdk/python/README.md), so any project
# that wants `tau.stellar` inside a workload container ships it the same way
# this example does.
#
# Keep stellar.py byte-identical to sdk/python/tau/stellar.py. An earlier
# revision of this example carried a lightly forked copy, and it silently went
# stale against the `taugrid-portal experiment` CLI: it still passed the
# removed `--description` flag to `experiments tag-run` and omitted the now
# required `--experiment` flag on `import jsonl`. Refresh it with:
#
#     cp sdk/python/tau/stellar.py \
#        examples/aks-cpu-quickstart/stellar-demo/tau/stellar.py
#
# Scope of this copy: local logging only. `Run.finish()` defaults to
# `sync=False` and this demo takes that default, so nothing here shells out to
# `taugrid-portal`. Enabling `sync=True` would reach `Run._resolve_binary()`,
# which imports `tau.workloads` -- absent from this trimmed package -- so the
# sync path needs the full SDK installed, not just this directory. Retrieval is
# done out-of-band with `kubectl cp` instead; see README.md.
from . import stellar  # noqa: F401
