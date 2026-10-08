# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Native Jupyter submission policy over the shared notebook SDK pipeline."""

from tau._notebook_submit import SubmitError, SubmitPlan, SubmitResult, _safe_name, submit_plan
from tau._notebook_submit import build_plan as _build_plan


def build_plan(**kwargs):
    return _build_plan(**kwargs, submission_mode="K8sJobMode", ttl_seconds=600)


def submit_notebook(**kwargs):
    client = kwargs.pop("client")
    backend = kwargs.pop("backend", None)
    return submit_plan(client=client, plan=build_plan(client=client, **kwargs), backend=backend)


__all__ = ["SubmitError", "SubmitPlan", "SubmitResult", "build_plan", "submit_plan", "submit_notebook", "_safe_name"]
