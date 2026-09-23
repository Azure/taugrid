#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/tau-kind-helpers.XXXXXX")"
mkdir "${TEST_ROOT}/bin"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT

export CALL_LOG="${TEST_ROOT}/calls"

cp "${REPO_ROOT}/scripts/ci/tests/fixtures/kind-tools.sh" "${TEST_ROOT}/bin/mock"
chmod +x "${TEST_ROOT}/bin/mock"
for tool in podman docker kind helm kubectl; do
  ln -s mock "${TEST_ROOT}/bin/${tool}"
done
export PATH="${TEST_ROOT}/bin:${PATH}"
unset KIND_EXPERIMENTAL_PROVIDER CONTAINERS_CGROUP_MANAGER KIND_TEST_FAILURE

assert_failure() {
  local expected_status="$1" expected_message="$2" output status=0
  shift 2
  output="$("$@" 2>&1)" || status=$?
  if [[ "${status}" -ne "${expected_status}" || "${output}" != *"${expected_message}"* ]]; then
    echo "unexpected failure ($status): $*: ${output}" >&2
    exit 1
  fi
}

# shellcheck source=../../lib/kind.sh
source "${REPO_ROOT}/scripts/lib/kind.sh"
# shellcheck source=../../lib/image-specs.sh
source "${REPO_ROOT}/scripts/lib/image-specs.sh"

[[ "$(taugrid_image_names)" == $'tau\ntaugrid-portal\ntau-core-controller\ntaugrid-metrics-collector' ]]
taugrid_image_spec taugrid-portal
[[ "${TAUGRID_IMAGE_REPOSITORY}" == taugrid-portal ]]
[[ "${TAUGRID_IMAGE_DOCKERFILE}" == images/taugrid-portal/Dockerfile ]]
[[ "${TAUGRID_IMAGE_CONTEXT}" == . ]]
[[ "${TAUGRID_IMAGE_SOURCE_PATHS[*]}" == "images/taugrid-portal/Dockerfile portal core" ]]
taugrid_image_spec taugrid-metrics-collector
[[ "${TAUGRID_IMAGE_REPOSITORY}" == taugrid-metrics-collector ]]
[[ "${TAUGRID_IMAGE_DOCKERFILE}" == images/taugrid-metrics-collector/Dockerfile ]]
[[ "${TAUGRID_IMAGE_CONTEXT}" == . ]]
[[ "${TAUGRID_IMAGE_SOURCE_PATHS[*]}" == "images/taugrid-metrics-collector/Dockerfile metrics/experiment-metrics-collector core" ]]
assert_failure 2 "unknown TauGrid image: unknown" taugrid_image_spec unknown

[[ "$(tau_kind_select_engine podman docker)" == podman ]]
[[ "$(tau_kind_select_engine "" docker)" == docker ]]
export KIND_EXPERIMENTAL_PROVIDER=podman
[[ "$(tau_kind_select_engine "" docker)" == podman ]]
unset KIND_EXPERIMENTAL_PROVIDER
[[ "$(tau_kind_select_engine "" auto)" == podman ]]
[[ "$(KIND_TEST_FAILURE=podman:info tau_kind_select_engine "" auto)" == docker ]]
assert_failure 2 "container engine must be podman or docker" tau_kind_select_engine invalid
assert_failure 2 "default container engine must be" tau_kind_select_engine "" invalid
KIND_TEST_FAILURE=info assert_failure 1 "neither Podman nor Docker" tau_kind_select_engine "" auto

[[ "$(tau_kind_qualify_image podman controller:dev)" == localhost/controller:dev ]]
[[ "$(tau_kind_qualify_image docker controller:dev)" == controller:dev ]]
[[ "$(tau_kind_qualify_image podman example.com/controller:dev)" == example.com/controller:dev ]]
[[ "$(tau_kind_expand_registry_image bash:5.2)" == docker.io/library/bash:5.2 ]]
[[ "$(tau_kind_expand_registry_image example.com/bash:5.2)" == example.com/bash:5.2 ]]

tau_kind_configure_provider podman
[[ "${KIND_EXPERIMENTAL_PROVIDER}" == podman ]]
[[ "${CONTAINERS_CGROUP_MANAGER}" == cgroupfs ]]
CONTAINERS_CGROUP_MANAGER=systemd
tau_kind_configure_provider podman
[[ "${CONTAINERS_CGROUP_MANAGER}" == systemd ]]
tau_kind_configure_provider docker
[[ -z "${KIND_EXPERIMENTAL_PROVIDER:-}" ]]

tau_kind_cluster_exists podman tau-test
tau_kind_cluster_exists docker tau-test
assert_failure 1 "" tau_kind_cluster_exists podman missing
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
assert_failure 1 "" grep -q '^kind load image-archive ' "${CALL_LOG}"
grep -q '^kind load docker-image controller:dev --name tau-test$' "${CALL_LOG}"
grep -q '^helm template release chart --namespace namespace --set feature=true --show-only templates/deployment.yaml$' "${CALL_LOG}"
grep -q '^kubectl --context kind-tau-test create --dry-run=client ' "${CALL_LOG}"

assert_load_failure() {
  KIND_TEST_FAILURE="$1" assert_failure 1 "$2" tau_kind_load_image podman tau-test localhost/controller:dev
}
assert_load_failure podman:image "not available on the host"
assert_load_failure podman:ps "failed to list Podman nodes"
assert_load_failure empty-nodes "has no Podman nodes"
assert_load_failure podman:save "failed to load"
assert_load_failure import "failed to load"

echo "Kind helper tests passed"
