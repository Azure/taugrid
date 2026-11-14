# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.58.0-cuda13.0@sha256:fc5f26891f1d9e170914f2052efbc49ceb9d50253acbb5955656455e120d8078

USER root

RUN python3 -m pip install --no-cache-dir \
    --index-url https://download.pytorch.org/whl/cu128 \
    --extra-index-url https://pypi.org/simple \
    "torch==2.7.1+cu128" \
    && python3 -m pip check

COPY scripts/images/ray-torch/check_image.py scripts/images/ray-torch/pip_spec.json scripts/images/ray-torch/test_runtime_env.py /opt/taugrid/ray-torch/

USER nonroot:nonroot

RUN python3 /opt/taugrid/ray-torch/check_image.py

ARG SOURCE_REVISION=dev
LABEL org.opencontainers.image.revision="${SOURCE_REVISION}"
