#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

tau_kind_need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    return 127
  fi
}

tau_kind_engine_healthy() {
  command -v "$1" >/dev/null 2>&1 && "$1" info >/dev/null 2>&1
}

tau_kind_select_engine() {
  local requested="${1:-}"
  local default_engine="${2:-auto}"

  if [[ -n "${requested}" ]]; then
    case "${requested}" in
      podman | docker) ;;
      *)
        echo "container engine must be podman or docker, got '${requested}'" >&2
        return 2
        ;;
    esac
    printf '%s\n' "${requested}"
    return
  fi

  if [[ "${KIND_EXPERIMENTAL_PROVIDER:-}" == "podman" ]]; then
    printf '%s\n' podman
    return
  fi

  case "${default_engine}" in
    podman | docker)
      printf '%s\n' "${default_engine}"
      ;;
    auto)
      if tau_kind_engine_healthy podman; then
        printf '%s\n' podman
      elif tau_kind_engine_healthy docker; then
        printf '%s\n' docker
      else
        echo "neither Podman nor Docker is installed and healthy" >&2
        return 1
      fi
      ;;
    *)
      echo "default container engine must be auto, podman, or docker, got '${default_engine}'" >&2
      return 2
      ;;
  esac
}

tau_kind_configure_provider() {
  local engine="$1"
  if [[ "${engine}" == "podman" ]]; then
    export KIND_EXPERIMENTAL_PROVIDER=podman
    export CONTAINERS_CGROUP_MANAGER="${CONTAINERS_CGROUP_MANAGER:-cgroupfs}"
  else
    unset KIND_EXPERIMENTAL_PROVIDER || true
  fi
}

tau_kind_qualify_image() {
  local engine="$1"
  local image="$2"
  if [[ "${engine}" == "podman" && "${image}" != */* ]]; then
    printf 'localhost/%s\n' "${image}"
  else
    printf '%s\n' "${image}"
  fi
}

tau_kind_expand_registry_image() {
  local image="$1"
  if [[ "${image}" != */* ]]; then
    printf 'docker.io/library/%s\n' "${image}"
  else
    printf '%s\n' "${image}"
  fi
}

tau_kind_cluster_exists() {
  local engine="$1"
  local cluster_name="$2"
  "${engine}" ps -a \
    --filter "label=io.x-k8s.kind.cluster=${cluster_name}" \
    --format '{{.Names}}' 2>/dev/null |
    grep -qx "${cluster_name}-control-plane"
}

tau_kind_create_cluster() {
  local cluster_name="$1"
  shift
  kind create cluster --name "${cluster_name}" "$@"
}

tau_kind_delete_cluster() {
  local cluster_name="$1"
  kind delete cluster --name "${cluster_name}"
}

tau_kind_load_image() {
  local engine="$1"
  local cluster_name="$2"
  local image="$3"
  local archive status

  if [[ "${engine}" != "podman" ]]; then
    kind load docker-image "${image}" --name "${cluster_name}"
    return
  fi

  archive="$(mktemp "${TMPDIR:-/tmp}/tau-kind-image.XXXXXX.tar")"
  status=0
  "${engine}" save --format docker-archive "${image}" >"${archive}" || status=$?
  if [[ "${status}" -eq 0 ]]; then
    kind load image-archive "${archive}" --name "${cluster_name}" || status=$?
  fi
  rm -f -- "${archive}"
  return "${status}"
}

tau_kind_render_image() {
  local release="$1"
  local chart="$2"
  local namespace="$3"
  local template="$4"
  shift 4

  helm template "${release}" "${chart}" \
    --namespace "${namespace}" \
    "$@" \
    --show-only "${template}" |
    kubectl create --dry-run=client -f - \
      -o jsonpath='{.spec.template.spec.containers[0].image}'
}
