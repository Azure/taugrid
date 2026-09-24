#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"

for consumer in \
  "${REPO_ROOT}/controllers/tau-core/scripts/kind-e2e.sh" \
  "${REPO_ROOT}/scripts/dev/kind-local.sh"; do
  if grep -Eq 'kind (get clusters|create cluster|delete cluster|load (docker-image|image-archive))' "${consumer}"; then
    echo "Kind lifecycle or image loading bypasses scripts/lib/kind.sh: ${consumer}" >&2
    exit 1
  fi
done

if grep -En 'kind (create|delete) cluster' "${REPO_ROOT}/cli/scripts/kind-smoke-e2e.sh" |
  grep -Ev 'run_with_timeout .* kind (create|delete) cluster'; then
  echo "CLI Kind lifecycle command bypasses bounded execution" >&2
  exit 1
fi

grep -Fq 'TAUGRID_NAMESPACE="${TAU_KIND_TAUGRID_NAMESPACE:-kueue-system}"' \
  "${REPO_ROOT}/cli/scripts/kind-smoke-e2e.sh" ||
  {
    echo "CLI Kind smoke must default the TauGrid release to kueue-system for KEC" >&2
    exit 1
  }

for kec_input in \
  'kubernetes.azure.com/mode=user' \
  'node.kubernetes.io/instance-type=Standard_D4s_v5'; do
  grep -Fq "$kec_input" "${REPO_ROOT}/cli/scripts/kind-smoke-e2e.sh" ||
    {
      echo "CLI Kind smoke must emulate KEC input label ${kec_input}" >&2
      exit 1
    }
done

grep -Fq 'workers: 1' "${REPO_ROOT}/examples/kind-smoke/tau-ray.yaml" ||
  {
    echo "single-node Kind Ray smoke must request one worker" >&2
    exit 1
  }
grep -Fq 'placement: independent' "${REPO_ROOT}/examples/kind-smoke/taucluster-profiles.yaml" ||
  {
    echo "single-node Kind Ray profile must use independent placement" >&2
    exit 1
  }

if grep -q 'ctr .* images remove' "${REPO_ROOT}/scripts/dev/kind-local.sh"; then
  echo "Kind development preemptively removes cached Podman images" >&2
  exit 1
fi

for marker in --dry-run=server --field-manager=taugrid-crds --for=condition=Established; do
  grep -q -- "${marker}" "${REPO_ROOT}/scripts/dev/kind-local.sh"
done

unset KIND_PLATFORM MAKEFLAGS MFLAGS MAKEOVERRIDES
remote_build() {
  make -s -n -C "${REPO_ROOT}" HOST_GOARCH=amd64 \
    KIND_EXECUTION=remote KIND_REMOTE_HOST=devbox "$@" kind-build-images
}

remote_default="$(remote_build)"
if grep -q 'KIND_PLATFORM=' <<<"${remote_default}"; then
  echo "remote Kind default unexpectedly forwards the client platform" >&2
  exit 1
fi
remote_arm64="$(remote_build KIND_PLATFORM=linux/arm64)"
grep -q 'KIND_PLATFORM="linux/arm64"' <<<"${remote_arm64}"
remote_env="$(KIND_PLATFORM=linux/amd64 remote_build)"
grep -q 'KIND_PLATFORM="linux/amd64"' <<<"${remote_env}"

echo "Kind consumer contracts (scripts and Makefile) passed"
