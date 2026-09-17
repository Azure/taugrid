#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/tau-kind-helpers.XXXXXX")"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT

mkdir -p "${TEST_ROOT}/bin"
CALL_LOG="${TEST_ROOT}/calls"
export CALL_LOG

cat >"${TEST_ROOT}/bin/podman" <<'EOF'
#!/bin/sh
set -eu
printf 'podman %s\n' "$*" >>"$CALL_LOG"
case "$1" in
  info)
    exit 0
    ;;
  ps)
    printf '%s\n' tau-test-control-plane
    exit 0
    ;;
  save)
    printf archive
    exit 0
    ;;
esac
EOF

cat >"${TEST_ROOT}/bin/docker" <<'EOF'
#!/bin/sh
set -eu
printf 'docker %s\n' "$*" >>"$CALL_LOG"
case "$1" in
  info)
    exit 0
    ;;
  ps)
    printf '%s\n' tau-test-control-plane
    exit 0
    ;;
esac
exit 1
EOF

cat >"${TEST_ROOT}/bin/kind" <<'EOF'
#!/bin/sh
set -eu
printf 'kind %s\n' "$*" >>"$CALL_LOG"
if [ "$1" = get ] && [ "$2" = clusters ]; then
  printf '%s\n' tau-test
fi
EOF

cat >"${TEST_ROOT}/bin/helm" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' 'apiVersion: apps/v1'
printf '%s\n' 'kind: Deployment'
printf '%s\n' 'spec:'
printf '%s\n' '  template:'
printf '%s\n' '    spec:'
printf '%s\n' '      containers:'
printf '%s\n' '        - image: registry.example.com/test:1'
EOF

cat >"${TEST_ROOT}/bin/kubectl" <<'EOF'
#!/bin/sh
set -eu
cat >/dev/null
printf '%s' 'registry.example.com/test:1'
EOF

chmod +x "${TEST_ROOT}/bin/"*
PATH="${TEST_ROOT}/bin:${PATH}"
export PATH

# shellcheck source=../../lib/kind.sh
source "${REPO_ROOT}/scripts/lib/kind.sh"
# shellcheck source=../../lib/image-specs.sh
source "${REPO_ROOT}/scripts/lib/image-specs.sh"

[[ "$(taugrid_image_names)" == $'tau\ntaugrid-portal\ntau-core-controller' ]]
taugrid_image_spec taugrid-portal
[[ "${TAUGRID_IMAGE_REPOSITORY}" == taugrid-portal ]]
[[ "${TAUGRID_IMAGE_DOCKERFILE}" == images/taugrid-portal/Dockerfile ]]
[[ "${TAUGRID_IMAGE_CONTEXT}" == . ]]
[[ "${TAUGRID_IMAGE_SOURCE_PATHS[*]}" == "images/taugrid-portal/Dockerfile portal core" ]]
if taugrid_image_spec unknown >/dev/null 2>&1; then
  echo "unknown image spec unexpectedly succeeded" >&2
  exit 1
fi

[[ "$(tau_kind_select_engine podman docker)" == podman ]]
[[ "$(tau_kind_select_engine "" docker)" == docker ]]
KIND_EXPERIMENTAL_PROVIDER=podman
export KIND_EXPERIMENTAL_PROVIDER
[[ "$(tau_kind_select_engine "" docker)" == podman ]]
unset KIND_EXPERIMENTAL_PROVIDER
[[ "$(tau_kind_select_engine "" auto)" == podman ]]

[[ "$(tau_kind_qualify_image podman controller:dev)" == localhost/controller:dev ]]
[[ "$(tau_kind_qualify_image docker controller:dev)" == controller:dev ]]
[[ "$(tau_kind_qualify_image podman example.com/controller:dev)" == example.com/controller:dev ]]
[[ "$(tau_kind_expand_registry_image bash:5.2)" == docker.io/library/bash:5.2 ]]
[[ "$(tau_kind_expand_registry_image example.com/bash:5.2)" == example.com/bash:5.2 ]]

tau_kind_configure_provider podman
[[ "${KIND_EXPERIMENTAL_PROVIDER}" == podman ]]
[[ "${CONTAINERS_CGROUP_MANAGER}" == cgroupfs ]]
tau_kind_configure_provider docker
[[ -z "${KIND_EXPERIMENTAL_PROVIDER:-}" ]]

tau_kind_cluster_exists podman tau-test
tau_kind_create_cluster tau-test --wait 10s
tau_kind_delete_cluster tau-test
tau_kind_load_image podman tau-test localhost/controller:dev
tau_kind_load_image docker tau-test controller:dev
[[ "$(tau_kind_render_image release chart namespace templates/deployment.yaml --set feature=true)" == registry.example.com/test:1 ]]

grep -q '^kind create cluster --name tau-test --wait 10s$' "${CALL_LOG}"
grep -q '^kind delete cluster --name tau-test$' "${CALL_LOG}"
grep -q '^kind load image-archive .* --name tau-test$' "${CALL_LOG}"
grep -q '^kind load docker-image controller:dev --name tau-test$' "${CALL_LOG}"

for consumer in \
  "${REPO_ROOT}/cli/scripts/kind-smoke-e2e.sh" \
  "${REPO_ROOT}/controllers/tau-core/scripts/kind-e2e.sh" \
  "${REPO_ROOT}/scripts/dev/kind-local.sh"; do
  if grep -Eq 'kind (get clusters|create cluster|delete cluster|load (docker-image|image-archive))' "${consumer}"; then
    echo "Kind lifecycle or image loading bypasses scripts/lib/kind.sh: ${consumer}" >&2
    exit 1
  fi
done

echo "Kind helper tests passed"
