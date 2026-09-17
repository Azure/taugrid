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
    printf '%s\n' tau-test-control-plane tau-test-worker
    exit 0
    ;;
  image)
    [ "$2" = inspect ]
    printf '%s\n' test-image-id
    exit 0
    ;;
  save)
    printf archive
    exit 0
    ;;
  exec)
    if [ "$2" = tau-test-control-plane ] && [ "$3" = crictl ]; then
      printf '%s\n' sha256:test-image-id
      exit 0
    fi
    if [ "$2" = tau-test-worker ] && [ "$3" = crictl ]; then
      exit 1
    fi
    if [ "$2" = -i ] && [ "$3" = tau-test-worker ] && [ "$4" = ctr ]; then
      cat >/dev/null
      exit 0
    fi
    exit 1
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
printf 'kubectl %s\n' "$*" >>"$CALL_LOG"
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
[[ "$(tau_kind_render_image kind-tau-test release chart namespace templates/deployment.yaml --set feature=true)" == registry.example.com/test:1 ]]

grep -q '^kind create cluster --name tau-test --wait 10s$' "${CALL_LOG}"
grep -q '^kind delete cluster --name tau-test$' "${CALL_LOG}"
grep -q "^podman image inspect --format {{.Id}} localhost/controller:dev$" "${CALL_LOG}"
grep -q "^podman exec tau-test-control-plane crictl inspecti -o go-template --template {{.status.id}} localhost/controller:dev$" "${CALL_LOG}"
grep -q "^podman exec tau-test-worker crictl inspecti -o go-template --template {{.status.id}} localhost/controller:dev$" "${CALL_LOG}"
grep -q '^podman save --format oci-archive localhost/controller:dev$' "${CALL_LOG}"
grep -q '^podman exec -i tau-test-worker ctr --namespace=k8s.io images import --all-platforms --digests -$' "${CALL_LOG}"
[[ "$(grep -c '^podman save ' "${CALL_LOG}")" -eq 1 ]]
if grep -q '^kind load image-archive ' "${CALL_LOG}"; then
  echo "Podman image loading unexpectedly used a temporary Kind archive" >&2
  exit 1
fi
if grep -q 'ctr .* images remove' "${REPO_ROOT}/scripts/dev/kind-local.sh"; then
  echo "Kind development preemptively removes cached Podman images" >&2
  exit 1
fi
grep -q '^kind load docker-image controller:dev --name tau-test$' "${CALL_LOG}"

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

grep -q '^kubectl --context kind-tau-test create --dry-run=client ' "${CALL_LOG}"

remote_default="$(make -s -n -C "${REPO_ROOT}" \
  KIND_EXECUTION=remote KIND_REMOTE_HOST=devbox kind-build-images)"
if grep -q 'KIND_PLATFORM=' <<<"${remote_default}"; then
  echo "remote Kind default unexpectedly forwards the client platform" >&2
  exit 1
fi
remote_arm64="$(make -s -n -C "${REPO_ROOT}" \
  KIND_EXECUTION=remote KIND_REMOTE_HOST=devbox KIND_PLATFORM=linux/arm64 \
  kind-build-images)"
grep -q 'KIND_PLATFORM="linux/arm64"' <<<"${remote_arm64}"

grep -q -- '--dry-run=server' "${REPO_ROOT}/scripts/dev/kind-local.sh"
grep -q -- '--field-manager=taugrid-crds' "${REPO_ROOT}/scripts/dev/kind-local.sh"
grep -q -- '--for=condition=Established' "${REPO_ROOT}/scripts/dev/kind-local.sh"

echo "Kind helper tests passed"
