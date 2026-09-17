#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
source "${REPO_ROOT}/scripts/lib/kind.sh"
source "${REPO_ROOT}/scripts/lib/image-specs.sh"
KIND_CONFIG="${REPO_ROOT}/examples/kind-smoke/kind-cluster.yaml"
CHART_DIR="${REPO_ROOT}/charts/taugrid"

ACTION="${1:-up}"
CLUSTER_NAME="${TAUGRID_KIND_CLUSTER_NAME:-taugrid-dev}"
KUBE_CONTEXT="${TAUGRID_KIND_CONTEXT:-kind-${CLUSTER_NAME}}"
NAMESPACE="${TAUGRID_KIND_NAMESPACE:-tau-system}"
RELEASE="${TAUGRID_KIND_RELEASE:-taugrid-dev}"
IMAGE_TAG="${TAUGRID_KIND_IMAGE_TAG:-dev}"
PLATFORM="${TAUGRID_KIND_PLATFORM:-linux/$(uname -m)}"
ENGINE="${TAUGRID_KIND_ENGINE:-}"
TAU_BIN="${TAUGRID_KIND_TAU_BIN:-${REPO_ROOT}/cli/bin/tau}"
TEST_WORKSPACE="${TAUGRID_KIND_TEST_WORKSPACE:-taugrid-kind-test}"
TEST_NAMESPACE="${TAUGRID_KIND_TEST_NAMESPACE:-${TEST_WORKSPACE}}"
TEST_PRINCIPAL="${TAUGRID_KIND_TEST_PRINCIPAL:-taugrid-kind-testers}"
MIN_MEMORY_MIB="${TAUGRID_KIND_MIN_MEMORY_MIB:-7680}"
KUEUE_IMAGE=""
KUBERAY_IMAGE=""

case "${PLATFORM}" in
  linux/arm64 | linux/aarch64) PLATFORM="linux/arm64" ;;
  linux/amd64 | linux/x86_64) PLATFORM="linux/amd64" ;;
esac

usage() {
  cat <<'EOF'
Usage: scripts/dev/kind-local.sh [build|create|load|install|restart|test-workspace|status|down]

  build   Build the local controller and Portal images.
  create  Create or reuse the Kind cluster.
  load    Load previously built images into the Kind cluster.
  install Install or update TauGrid from the local chart.
  restart Restart TauGrid pods onto the newly loaded local images.
  test-workspace
          Create a TauWorkspace and verify real Kueue reconciliation.
  status  Show TauGrid deployments, pods, and local image references.
  down    Delete the local Kind cluster.

Use the repository Makefile for the complete workflow: make kind-up.
Podman is preferred when available and healthy. Docker is the fallback.
Override selection with TAUGRID_KIND_ENGINE=podman or docker.

Useful overrides:
  TAUGRID_KIND_CLUSTER_NAME=taugrid-dev
  TAUGRID_KIND_IMAGE_TAG=dev
  TAUGRID_KIND_PLATFORM=linux/arm64
EOF
}

select_engine() {
  ENGINE="$(tau_kind_select_engine "${ENGINE}" auto)"
  if ! tau_kind_engine_healthy "${ENGINE}"; then
    echo "${ENGINE} is selected but is unavailable or unhealthy" >&2
    exit 1
  fi
}

configure_kind_provider() {
  tau_kind_configure_provider "${ENGINE}"
}

warn_engine_memory() {
  local memory_format memory_bytes memory_mib
  if [[ "${ENGINE}" == "podman" ]]; then
    memory_format='{{.Host.MemTotal}}'
  else
    memory_format='{{.MemTotal}}'
  fi

  memory_bytes="$("${ENGINE}" info --format "${memory_format}" 2>/dev/null || true)"
  if [[ ! "${memory_bytes}" =~ ^[0-9]+$ ]]; then
    echo "warning: could not determine ${ENGINE} memory; continuing" >&2
    return
  fi

  memory_mib=$((memory_bytes / 1024 / 1024))
  if (( memory_mib < MIN_MEMORY_MIB )); then
    echo "warning: ${ENGINE} reports ${memory_mib} MiB; the full TauGrid Kind stack may be OOM-killed below ${MIN_MEMORY_MIB} MiB" >&2
    if [[ "${ENGINE}" == "podman" ]]; then
      echo "warning: increase a Podman VM with: podman machine stop && podman machine set --memory 8192 && podman machine start" >&2
    fi
  fi
}

controller_repository() {
  taugrid_image_spec tau-core-controller
  tau_kind_qualify_image "${ENGINE}" "${TAUGRID_IMAGE_REPOSITORY}"
}

portal_repository() {
  taugrid_image_spec taugrid-portal
  tau_kind_qualify_image "${ENGINE}" "${TAUGRID_IMAGE_REPOSITORY}"
}

cluster_exists() {
  tau_kind_cluster_exists "${ENGINE}" "${CLUSTER_NAME}"
}

create_cluster() {
  if cluster_exists; then
    echo "Reusing Kind cluster ${CLUSTER_NAME}"
    return
  fi

  echo "Creating Kind cluster ${CLUSTER_NAME} with ${ENGINE}"
  tau_kind_create_cluster "${CLUSTER_NAME}" --config "${KIND_CONFIG}"
}

resolve_dependency_images() {
  "${REPO_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh" "${CHART_DIR}"
  KUEUE_IMAGE="$(tau_kind_render_image \
    "${RELEASE}" "${CHART_DIR}" "${NAMESPACE}" \
    charts/kueue/templates/manager/manager.yaml \
    --set components.gpuMonitoring.enabled=false \
    --set baselineQueue.enabled=false)"
  KUBERAY_IMAGE="$(tau_kind_render_image \
    "${RELEASE}" "${CHART_DIR}" "${NAMESPACE}" \
    charts/kuberay-operator/templates/deployment.yaml \
    --set components.gpuMonitoring.enabled=false \
    --set baselineQueue.enabled=false)"

  if [[ -z "${KUEUE_IMAGE}" || -z "${KUBERAY_IMAGE}" ]]; then
    echo "failed to resolve dependency images from the TauGrid chart" >&2
    exit 1
  fi
}

build_images() {
  local controller_image controller_dockerfile controller_context
  local portal_image portal_dockerfile portal_context

  taugrid_image_spec tau-core-controller
  controller_image="$(tau_kind_qualify_image "${ENGINE}" "${TAUGRID_IMAGE_REPOSITORY}"):${IMAGE_TAG}"
  controller_dockerfile="${REPO_ROOT}/${TAUGRID_IMAGE_DOCKERFILE}"
  controller_context="${REPO_ROOT}/${TAUGRID_IMAGE_CONTEXT}"

  taugrid_image_spec taugrid-portal
  portal_image="$(tau_kind_qualify_image "${ENGINE}" "${TAUGRID_IMAGE_REPOSITORY}"):${IMAGE_TAG}"
  portal_dockerfile="${REPO_ROOT}/${TAUGRID_IMAGE_DOCKERFILE}"
  portal_context="${REPO_ROOT}/${TAUGRID_IMAGE_CONTEXT}"

  echo "Building ${controller_image} for ${PLATFORM}"
  if ! "${ENGINE}" build \
    --network host \
    --platform "${PLATFORM}" \
    --file "${controller_dockerfile}" \
    --tag "${controller_image}" \
    "${controller_context}"; then
    image_build_failed
    return 1
  fi

  echo "Building ${portal_image} for ${PLATFORM}"
  if ! "${ENGINE}" build \
    --network host \
    --platform "${PLATFORM}" \
    --file "${portal_dockerfile}" \
    --tag "${portal_image}" \
    "${portal_context}"; then
    image_build_failed
    return 1
  fi
}

image_build_failed() {
  echo "${ENGINE} image build failed" >&2
  if [[ "${ENGINE}" == "podman" && "$(uname -s)" == "Darwin" ]]; then
    echo "Check Podman storage with: podman system df" >&2
    echo "On macOS, increase the VM disk with: podman machine stop && podman machine set --disk-size 100 && podman machine start" >&2
  fi
}

load_images() {
  local controller_image portal_image image node
  local -a images
  resolve_dependency_images
  controller_image="$(controller_repository):${IMAGE_TAG}"
  portal_image="$(portal_repository):${IMAGE_TAG}"
  images=(
    "${controller_image}"
    "${portal_image}"
    "${KUEUE_IMAGE}"
    "${KUBERAY_IMAGE}"
  )

  for image in "${KUEUE_IMAGE}" "${KUBERAY_IMAGE}"; do
    echo "Refreshing ${image} through the host container engine"
    "${ENGINE}" pull "${image}"
  done

  if [[ "${ENGINE}" == "podman" ]]; then
    while IFS= read -r node; do
      for image in "${images[@]}"; do
        "${ENGINE}" exec "${node}" ctr --namespace k8s.io images remove "${image}" \
          >/dev/null 2>&1 || true
      done
    done < <(
      "${ENGINE}" ps -a \
        --filter "label=io.x-k8s.kind.cluster=${CLUSTER_NAME}" \
        --format '{{.Names}}'
    )
    for image in "${images[@]}"; do
      tau_kind_load_image "${ENGINE}" "${CLUSTER_NAME}" "${image}"
    done
  else
    for image in "${images[@]}"; do
      tau_kind_load_image "${ENGINE}" "${CLUSTER_NAME}" "${image}"
    done
  fi
}

install_taugrid() {
  local controller_repo portal_repo
  local -a helm_args
  controller_repo="$(controller_repository)"
  portal_repo="$(portal_repository)"

  "${REPO_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh" "${CHART_DIR}"

  helm_args=(
    upgrade --install "${RELEASE}" "${CHART_DIR}"
    --kube-context "${KUBE_CONTEXT}" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --wait \
    --timeout 5m \
    --set components.gpuMonitoring.enabled=false \
    --set baselineQueue.gpu.enabled=false \
    --set "tau-core-controller.image.repository=${controller_repo}" \
    --set "tau-core-controller.image.tag=${IMAGE_TAG}" \
    --set tau-core-controller.image.pullPolicy=Never \
    --set "taugrid-core.portal.image.repository=${portal_repo}" \
    --set "taugrid-core.portal.image.tag=${IMAGE_TAG}" \
    --set taugrid-core.portal.image.pullPolicy=Never
  )

  echo "Bootstrapping TauGrid controllers and CRDs in ${NAMESPACE}"
  helm "${helm_args[@]}" --set baselineQueue.enabled=false

  echo "Installing TauGrid queue policy"
  helm "${helm_args[@]}" --reset-values
}

show_status() {
  kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    get deployments,pods -o wide
  echo
  kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    get pods \
    -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[*].image,READY:.status.containerStatuses[*].ready'
}

restart_local_deployments() {
  local deployment
  for deployment in tau-core-controller tau-portal; do
    kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
      rollout restart "deployment/${deployment}"
  done
  for deployment in tau-core-controller tau-portal; do
    kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
      rollout status "deployment/${deployment}" --timeout=180s
  done
}

test_workspace() (
  local failed=1
  local existing phase local_queue_cluster_queue local_queue_api_version

  cleanup() {
    local status=$?
    if [[ "${failed}" == "1" ]]; then
      echo "Workspace integration test failed; dumping diagnostics" >&2
      kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
        get "workspaces.tau.azure.com/${TEST_WORKSPACE}" -o yaml >&2 || true
      kubectl --context "${KUBE_CONTEXT}" --namespace "${TEST_NAMESPACE}" \
        get namespace,localqueue.kueue.x-k8s.io,role,rolebinding,serviceaccount -o wide >&2 || true
      kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
        logs deployment/tau-core-controller --tail=200 >&2 || true
    fi

    kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
      delete "workspace.tau.azure.com/${TEST_WORKSPACE}" \
      --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
    kubectl --context "${KUBE_CONTEXT}" \
      delete namespace "${TEST_NAMESPACE}" \
      --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
    return "${status}"
  }
  trap cleanup EXIT

  if [[ ! -x "${TAU_BIN}" ]]; then
    echo "Tau CLI not found or not executable: ${TAU_BIN}" >&2
    exit 1
  fi

  existing="$(kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    get workspaces.tau.azure.com \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"
  if [[ -n "${existing}" && "${existing}" != "${TEST_WORKSPACE}" ]]; then
    echo "another TauWorkspace already exists in ${NAMESPACE}: ${existing}" >&2
    echo "the v0 cluster supports one workspace; remove it before running this test" >&2
    exit 1
  fi

  kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    delete "workspace.tau.azure.com/${TEST_WORKSPACE}" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null
  kubectl --context "${KUBE_CONTEXT}" \
    delete namespace "${TEST_NAMESPACE}" \
    --ignore-not-found --wait=true --timeout=120s >/dev/null

  echo "Creating TauWorkspace ${TEST_WORKSPACE}"
  "${TAU_BIN}" workspace create "${TEST_WORKSPACE}" \
    --context "${KUBE_CONTEXT}" \
    --system-namespace "${NAMESPACE}" \
    --namespace "${TEST_NAMESPACE}" \
    --queue jobqueue \
    --principal-name "${TEST_PRINCIPAL}" \
    --apply

  kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    wait "workspace.tau.azure.com/${TEST_WORKSPACE}" \
    --for=jsonpath='{.status.phase}'=Ready \
    --timeout=120s

  phase="$(kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    get "workspace.tau.azure.com/${TEST_WORKSPACE}" \
    -o jsonpath='{.status.phase}')"
  [[ "${phase}" == "Ready" ]]

  kubectl --context "${KUBE_CONTEXT}" --namespace "${TEST_NAMESPACE}" \
    wait localqueue.kueue.x-k8s.io/jobqueue \
    --for=condition=Active \
    --timeout=120s

  local_queue_api_version="$(kubectl --context "${KUBE_CONTEXT}" --namespace "${TEST_NAMESPACE}" \
    get localqueue.kueue.x-k8s.io/jobqueue -o jsonpath='{.apiVersion}')"
  local_queue_cluster_queue="$(kubectl --context "${KUBE_CONTEXT}" --namespace "${TEST_NAMESPACE}" \
    get localqueue.kueue.x-k8s.io/jobqueue -o jsonpath='{.spec.clusterQueue}')"
  [[ "${local_queue_api_version}" == "kueue.x-k8s.io/v1beta2" ]]
  [[ "${local_queue_cluster_queue}" == "jobqueue" ]]

  kubectl --context "${KUBE_CONTEXT}" get namespace "${TEST_NAMESPACE}" \
    -o jsonpath='{.metadata.labels.kueue\.x-k8s\.io/default-local-queue}' |
    grep -qx jobqueue

  kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    delete "workspace.tau.azure.com/${TEST_WORKSPACE}" \
    --wait=true --timeout=120s

  if kubectl --context "${KUBE_CONTEXT}" --namespace "${TEST_NAMESPACE}" \
    get localqueue.kueue.x-k8s.io/jobqueue >/dev/null 2>&1; then
    echo "workspace LocalQueue remained after TauWorkspace deletion" >&2
    exit 1
  fi

  failed=0
  echo "TauWorkspace and Kueue reconciliation test passed"
)

case "${ACTION}" in
  help | -h | --help)
    usage
    exit 0
    ;;
  build | create | load | install | down)
    tau_kind_need kind
    select_engine
    configure_kind_provider
    ;;
  status | restart | test-workspace)
    tau_kind_need kubectl
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

case "${ACTION}" in
  build)
    build_images
    ;;
  create)
    warn_engine_memory
    create_cluster
    ;;
  load)
    tau_kind_need kubectl
    tau_kind_need helm
    if ! cluster_exists; then
      echo "Kind cluster ${CLUSTER_NAME} does not exist; run 'make kind-create' first" >&2
      exit 1
    fi
    load_images
    echo "Local images loaded into ${CLUSTER_NAME}"
    ;;
  install)
    tau_kind_need kubectl
    tau_kind_need helm
    if ! cluster_exists; then
      echo "Kind cluster ${CLUSTER_NAME} does not exist; run 'make kind-create' first" >&2
      exit 1
    fi
    install_taugrid
    echo "Portal: kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} port-forward svc/tau-portal 8080:80"
    ;;
  restart)
    restart_local_deployments
    ;;
  test-workspace)
    test_workspace
    ;;
  status)
    show_status
    ;;
  down)
    tau_kind_delete_cluster "${CLUSTER_NAME}"
    ;;
esac
