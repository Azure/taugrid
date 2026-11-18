#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

image="${1:?image reference is required}"
container="$(docker create --network none --shm-size=256m "${image}" \
  python3 /opt/taugrid/ray-torch/test_runtime_env.py)"
trap 'docker rm -f "${container}" >/dev/null' EXIT
docker start --attach "${container}"
status="$(docker inspect --format '{{.State.ExitCode}}' "${container}")"
if [[ "${status}" != 0 ]]; then
  echo "runtime-env image check exited ${status}" >&2
  exit 1
fi
writable_bytes="$(docker inspect --size --format '{{.SizeRw}}' "${container}")"
if [[ ! "${writable_bytes}" =~ ^[0-9]+$ ]] || (( writable_bytes > 536870912 )); then
  echo "container writable layer exceeds 512 MiB: ${writable_bytes} bytes" >&2
  exit 1
fi
printf '{"container_writable_bytes":%s,"max_writable_bytes":536870912}\n' "${writable_bytes}"
