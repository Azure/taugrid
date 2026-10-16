#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly INPUT_DIR="${1:-${UNBOUNDED_STABLE_REPORT_INPUT_DIR:-}}"
readonly OUTPUT_FILE="${2:-${UNBOUNDED_STABLE_REPORT_OUTPUT_FILE:-}}"

fail() {
  echo "TauGrid unbounded-stable nightly report failed: $*" >&2
  exit 1
}

[ -d "$INPUT_DIR" ] || fail "input directory is missing: ${INPUT_DIR}"
[ -n "$OUTPUT_FILE" ] || fail "output file is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

mkdir -p "$(dirname "$OUTPUT_FILE")"

find_first() {
  find "$INPUT_DIR" -type f -name "$1" -print | LC_ALL=C sort | head -n 1
}

markdown_cell() {
  local value="${1:-}"
  value="${value//$'\r'/ }"
  value="${value//$'\n'/<br>}"
  value="${value//|/\\|}"
  printf '%s' "$value"
}

job_result() {
  local value="${1:-Unknown}"
  case "$value" in
    Succeeded) printf 'PASS' ;;
    SucceededWithIssues) printf 'WARN' ;;
    Failed) printf 'FAIL' ;;
    Skipped) printf 'SKIP' ;;
    Canceled) printf 'CANCELED' ;;
    *) printf '%s' "$value" ;;
  esac
}

image_file="$(find_first images.json)"
deployment_file="$(find_first deployment-result.json)"
smoke_file="$(find_first tau-native-smoke-result.json)"

{
  echo "# TauGrid unbounded-stable nightly deployment"
  echo
  echo "## Run"
  echo
  echo "| Field | Value |"
  echo "|---|---|"
  printf '| Build | %s |\n' "$(markdown_cell "${BUILD_BUILDNUMBER:-${BUILD_BUILDID:-unknown}}")"
  printf '| Build ID | %s |\n' "$(markdown_cell "${BUILD_BUILDID:-unknown}")"
  printf "| Source | %s @ \`%s\` |\n" \
    "$(markdown_cell "${BUILD_SOURCEBRANCH:-unknown}")" \
    "$(markdown_cell "${BUILD_SOURCEVERSION:-unknown}")"
  printf '| Environment | **unbounded-stable** |\n'
  printf '| Report generated | %s |\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '| Contract job | **%s** |\n' "$(job_result "${PREFLIGHT_JOB_RESULT:-Unknown}")"
  printf '| Image publication job | **%s** |\n' "$(job_result "${BUILD_IMAGES_JOB_RESULT:-Unknown}")"
  printf '| Deployment job | **%s** |\n' "$(job_result "${DEPLOY_JOB_RESULT:-Unknown}")"
  echo

  echo "## Deployment"
  echo
  if [[ -n "$deployment_file" ]]; then
    status="$(jq -r '.status // "unknown"' "$deployment_file")"
    reason="$(jq -r '.reason // ""' "$deployment_file")"
    printf -- '- Outcome: **%s**' "$(markdown_cell "$status")"
    [[ -z "$reason" ]] || printf ' - %s' "$(markdown_cell "$reason")"
    echo
    printf -- '- Source commit: `%s`\n' \
      "$(markdown_cell "$(jq -r '.source_version // "unknown"' "$deployment_file")")"
    printf -- '- Immutable image tag: `%s`\n' \
      "$(markdown_cell "$(jq -r '.image_tag // "unknown"' "$deployment_file")")"
    previous_revision="$(jq -r '.previous_revision // ""' "$deployment_file")"
    current_revision="$(jq -r '.current_revision // ""' "$deployment_file")"
    if [[ -n "$previous_revision" || -n "$current_revision" ]]; then
      printf -- '- Helm revision: `%s` -> `%s`\n' \
        "$(markdown_cell "${previous_revision:-unknown}")" \
        "$(markdown_cell "${current_revision:-unknown}")"
    fi
  else
    echo "- No deployment result was produced. Deployment job outcome: **$(job_result "${DEPLOY_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Tau-native workload smoke"
  echo
  if [[ -n "$smoke_file" ]]; then
    smoke_status="$(jq -r '.status // "unknown"' "$smoke_file")"
    smoke_reason="$(jq -r '.reason // ""' "$smoke_file")"
    printf -- '- Outcome: **%s**' "$(markdown_cell "$smoke_status")"
    [[ -z "$smoke_reason" ]] || printf ' - %s' "$(markdown_cell "$smoke_reason")"
    echo
    printf -- '- Run: `%s`\n' \
      "$(markdown_cell "$(jq -r '.run_name // "unknown"' "$smoke_file")")"
    printf -- '- Workspace/profile: `%s` / `%s`\n' \
      "$(markdown_cell "$(jq -r '.workspace // "unknown"' "$smoke_file")")" \
      "$(markdown_cell "$(jq -r '.profile // "unknown"' "$smoke_file")")"
    printf -- '- Tau lifecycle state: `%s`\n' \
      "$(markdown_cell "$(jq -r '.lifecycle_state // "unknown"' "$smoke_file")")"
    printf -- '- Config SHA-256: `%s`\n' \
      "$(markdown_cell "$(jq -r '.config_sha256 // "unknown"' "$smoke_file")")"
    if jq -e '.topology != null' "$smoke_file" >/dev/null; then
      printf -- '- Topology admission: workload `%s` PodSet `%s` admitted by `%s`; `%s` using `%s` for %s workers\n' \
        "$(markdown_cell "$(jq -r '.topology.workload // "unknown"' "$smoke_file")")" \
        "$(markdown_cell "$(jq -r '.topology.pod_set // "unknown"' "$smoke_file")")" \
        "$(markdown_cell "$(jq -r '.topology.cluster_queue // "unknown"' "$smoke_file")")" \
        "$(markdown_cell "$(jq -r '.topology.level // "unknown"' "$smoke_file")")" \
        "$(markdown_cell "$(jq -r '.topology.flavor // "unknown"' "$smoke_file")")" \
        "$(markdown_cell "$(jq -r '.topology.assigned_workers // 0' "$smoke_file")")"
      printf -- '- Topology domains: `%s`\n' \
        "$(markdown_cell "$(jq -r \
          '.topology.domains | map((.values | join("/")) + "=" + (.count | tostring)) | join(", ")' \
          "$smoke_file")")"
      printf -- '- Ray worker nodes: `%s`\n' \
        "$(markdown_cell "$(jq -r '.topology.pod_nodes | join(", ")' "$smoke_file")")"
    fi
  else
    echo "- No Tau-native smoke result was produced. Deployment job outcome: **$(job_result "${DEPLOY_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Published image contract"
  echo
  if [[ -n "$image_file" ]]; then
    printf -- '- Source commit: `%s`\n' \
      "$(markdown_cell "$(jq -r '.source_version // "unknown"' "$image_file")")"
    printf -- '- Immutable tag: `%s`\n' \
      "$(markdown_cell "$(jq -r '.tag // "unknown"' "$image_file")")"
    echo "- Components: tau-core-controller, taugrid-portal, gpu-metrics-collector"
  else
    echo "- No image contract was produced. Image publication job outcome: **$(job_result "${BUILD_IMAGES_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Failure behavior"
  echo
  echo "- Helm upgrade requests atomic rollback and waits for both Helm and TauGrid readiness."
  echo "- Deployment diagnostics are published even when rendering, upgrade, rollback, or validation fails."
  echo "- The live operator-supplied Helm values are reused without publishing the values file as an artifact."
} >"$OUTPUT_FILE"

echo "TauGrid unbounded-stable nightly report written to ${OUTPUT_FILE}"
