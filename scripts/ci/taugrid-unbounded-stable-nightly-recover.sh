#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

require_value() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "${name} must be set" >&2
    exit 1
  fi
}

require_value TAUGRID_RELEASE
require_value TAUGRID_SYSTEM_NAMESPACE

readonly KUBE_CONTEXT="${TAUGRID_KUBE_CONTEXT:-unbounded-stable}"
readonly TAU_CLI="${TAUGRID_CLI:-cli/bin/tau}"
readonly DIAGNOSTICS_DIR="${TAUGRID_DIAGNOSTICS_DIR:-.}"

release_state() {
  local inventory matches
  inventory="$(
    helm list \
      --namespace "${TAUGRID_SYSTEM_NAMESPACE}" \
      --kube-context "${KUBE_CONTEXT}" \
      --deployed \
      --failed \
      --pending-install \
      --pending-upgrade \
      --pending-rollback \
      --uninstalled \
      --uninstalling \
      --superseded \
      --output json
  )"
  matches="$(
    jq --arg release "${TAUGRID_RELEASE}" \
      '[.[] | select(.name == $release)]' <<<"${inventory}"
  )"
  if [[ "$(jq 'length' <<<"${matches}")" -gt 1 ]]; then
    echo "multiple Helm releases named ${TAUGRID_RELEASE} exist in ${TAUGRID_SYSTEM_NAMESPACE}" >&2
    return 1
  fi
  if [[ "$(jq 'length' <<<"${matches}")" -eq 0 ]]; then
    jq -n '{exists: false, revision: "0", status: ""}'
    return
  fi

  local revision status
  revision="$(jq -r '.[0].revision | tostring' <<<"${matches}")"
  status="$(jq -r '.[0].status' <<<"${matches}")"
  if [[ ! "${revision}" =~ ^[1-9][0-9]*$ ]]; then
    echo "Helm returned an invalid revision for ${TAUGRID_RELEASE}: ${revision}" >&2
    return 1
  fi
  jq -n \
    --arg revision "${revision}" \
    --arg status "${status}" \
    '{exists: true, revision: $revision, status: $status}'
}

recover() {
  require_value TAUGRID_PREVIOUS_REVISION
  if [[ ! "${TAUGRID_PREVIOUS_REVISION}" =~ ^[0-9]+$ ]]; then
    echo "TAUGRID_PREVIOUS_REVISION must be a non-negative integer" >&2
    exit 1
  fi
  mkdir -p "${DIAGNOSTICS_DIR}"

  local state
  state="$(release_state)"
  if [[ "${TAUGRID_PREVIOUS_REVISION}" == "0" ]]; then
    if [[ "$(jq -r '.exists' <<<"${state}")" == "true" ]]; then
      helm uninstall "${TAUGRID_RELEASE}" \
        --namespace "${TAUGRID_SYSTEM_NAMESPACE}" \
        --kube-context "${KUBE_CONTEXT}" \
        --wait \
        --timeout 20m \
        >"${DIAGNOSTICS_DIR}/recovery.txt" 2>&1
    fi
    state="$(release_state)"
    if [[ "$(jq -r '.exists' <<<"${state}")" != "false" ]]; then
      echo "the rejected first installation still exists after recovery" >&2
      exit 1
    fi
    echo "removed the rejected first installation"
    return
  fi

  if [[ "$(jq -r '.exists' <<<"${state}")" != "true" ]]; then
    echo "cannot restore revision ${TAUGRID_PREVIOUS_REVISION}: release ${TAUGRID_RELEASE} is absent" >&2
    exit 1
  fi
  helm rollback "${TAUGRID_RELEASE}" "${TAUGRID_PREVIOUS_REVISION}" \
    --namespace "${TAUGRID_SYSTEM_NAMESPACE}" \
    --kube-context "${KUBE_CONTEXT}" \
    --wait \
    --timeout 20m \
    >"${DIAGNOSTICS_DIR}/recovery.txt" 2>&1
  "${TAU_CLI}" cluster validate installation \
    --context "${KUBE_CONTEXT}" \
    --release "${TAUGRID_RELEASE}" \
    --namespace "${TAUGRID_SYSTEM_NAMESPACE}" \
    --timeout 10m \
    >"${DIAGNOSTICS_DIR}/recovery-validation.txt" 2>&1
  echo "restored and validated revision ${TAUGRID_PREVIOUS_REVISION}"
}

case "${1:-}" in
  inspect)
    release_state
    ;;
  recover)
    recover
    ;;
  *)
    echo "usage: $0 <inspect|recover>" >&2
    exit 2
    ;;
esac
