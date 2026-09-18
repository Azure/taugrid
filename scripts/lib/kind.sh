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
  local host_id node node_id nodes node_count=0

  if [[ "${engine}" != "podman" ]]; then
    kind load docker-image "${image}" --name "${cluster_name}"
    return
  fi

  if ! host_id="$("${engine}" image inspect --format '{{.Id}}' "${image}")"; then
    echo "Podman image ${image} is not available on the host" >&2
    return 1
  fi
  host_id="${host_id#sha256:}"
  if ! nodes="$(
    "${engine}" ps -a \
      --filter "label=io.x-k8s.kind.cluster=${cluster_name}" \
      --format '{{.Names}}'
  )"; then
    echo "failed to list Podman nodes for Kind cluster ${cluster_name}" >&2
    return 1
  fi

  while IFS= read -r node; do
    [[ -n "${node}" ]] || continue
    node_count=$((node_count + 1))
    node_id="$("${engine}" exec "${node}" \
      crictl inspecti -o go-template --template '{{.status.id}}' "${image}" \
      2>/dev/null || true)"
    if [[ "${node_id#sha256:}" == "${host_id}" ]]; then
      echo "Image ${image} is already current on ${node}; skipping"
      continue
    fi
    echo "Loading ${image} into ${node}"
    if ! (
      set -o pipefail
      "${engine}" save --format oci-archive "${image}" |
        "${engine}" exec -i "${node}" \
          ctr --namespace=k8s.io images import --all-platforms --digests -
    ); then
      echo "failed to load ${image} into ${node}" >&2
      return 1
    fi
  done <<<"${nodes}"

  if [[ "${node_count}" -eq 0 ]]; then
    echo "Kind cluster ${cluster_name} has no Podman nodes" >&2
    return 1
  fi
}

tau_kind_render_image() {
  local context="$1"
  local release="$2"
  local chart="$3"
  local namespace="$4"
  local template="$5"
  shift 5

  helm template "${release}" "${chart}" \
    --namespace "${namespace}" \
    "$@" \
    --show-only "${template}" |
    kubectl --context "${context}" create --dry-run=client -f - \
      -o jsonpath='{.spec.template.spec.containers[0].image}'
}
