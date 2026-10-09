# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# TauGrid notebook runtime: ray plus the notebook executor.
#
# The base AI runtime image ships ray but no notebook execution stack, and both
# pods and image builds can be cut off from PyPI on a locked-down network, so the
# executor is baked in at build time. Populate wheels/ (gitignored) to build
# offline:
#
#   python -m pip download --dest images/notebook-runtime/wheels \
#       "nbformat>=5.10" "nbconvert>=7.16" "ipykernel>=6.29"
#   docker build -t taugrid-notebook-runtime:local images/notebook-runtime
#
# With an empty wheels/ the build falls back to the package index. Publish the
# image through the normal pipeline; notebook submission stays disabled until a
# runtime image like this is published and recorded.

FROM mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.56.0-cuda13.0

COPY wheels/ /wheels/

RUN if ls /wheels/*.whl >/dev/null 2>&1; then \
        python3 -m pip install --no-cache-dir --no-index --find-links=/wheels \
            "nbformat>=5.10" "nbconvert>=7.16" "ipykernel>=6.29"; \
    else \
        python3 -m pip install --no-cache-dir \
            "nbformat>=5.10" "nbconvert>=7.16" "ipykernel>=6.29"; \
    fi

# Prove the executor is importable at build time, so a bad image fails here
# rather than in a user's job.
RUN python3 -c "import nbformat, nbconvert, ipykernel; print('notebook executor ready')"
