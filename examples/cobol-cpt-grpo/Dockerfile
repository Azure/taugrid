# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM vllm/vllm-openai:v0.28.0@sha256:61fc8a896b0a4fbbbdc063bc4b0dbc25ce98e02b5050c24aeb7830ac02039b14

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        gnucobol3=3.1.2-5.1ubuntu1 \
        wget=1.21.4-1ubuntu4.5 && \
    rm -rf /var/lib/apt/lists/*

ARG PIP_INDEX_URL=https://pypi.org/simple
COPY image-requirements.txt /tmp/image-requirements.txt
RUN pip install --no-cache-dir -r /tmp/image-requirements.txt && \
    rm /tmp/image-requirements.txt
