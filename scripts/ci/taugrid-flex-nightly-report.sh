#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly INPUT_DIR="${1:-${FLEX_NIGHTLY_REPORT_INPUT_DIR:-}}"
readonly OUTPUT_FILE="${2:-${FLEX_NIGHTLY_REPORT_OUTPUT_FILE:-}}"

fail() {
  echo "TauGrid Flex nightly report failed: $*" >&2
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

contract_file="$(find_first hardware-matrix.json)"
matrix_file="$(find_first matrix-results.jsonl)"
postflight_file="$(find_first postflight-status.json)"

{
  echo "# TauGrid Flex nightly report"
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
  printf '| Profile | %s |\n' "$(markdown_cell "${FLEX_NIGHTLY_PROFILE:-unknown}")"
  printf '| Report generated | %s |\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '| Preflight job | **%s** |\n' "$(job_result "${PREFLIGHT_JOB_RESULT:-Unknown}")"
  printf '| Hardware matrix job | **%s** |\n' "$(job_result "${MATRIX_JOB_RESULT:-Unknown}")"
  if [[ "${FLEX_NIGHTLY_RDMA_ENABLED:-false}" == "true" ||
    "${FLEX_NIGHTLY_RDMA_ENABLED:-false}" == "True" ]]; then
    printf '| H200 RDMA job | **%s** |\n' "$(job_result "${RDMA_JOB_RESULT:-Unknown}")"
  fi
  echo

  echo "## Hardware preflight"
  echo
  if [[ -n "$contract_file" ]]; then
    echo "| Target | Status | Ready nodes | GPU capacity | Maximum free per node | Detail |"
    echo "|---|---|---:|---:|---:|---|"
    while IFS=$'\t' read -r name status ready available requested total maximum detail; do
      printf '| %s | **%s** | %s | %s free / %s requested / %s total | %s | %s |\n' \
        "$(markdown_cell "$name")" \
        "$(markdown_cell "$status")" \
        "$ready" "$available" "$requested" "$total" "$maximum" \
        "$(markdown_cell "$detail")"
    done < <(
      jq -r '
        .targets[]
        | [
            .name,
            .status,
            (.ready_nodes // 0),
            (.gpu_available // 0),
            (.gpu_requested // 0),
            (.gpu_total // 0),
            (.max_gpu_available_per_node // 0),
            (.reason // ((.capability_violations // []) | join("; ")) // "")
          ]
        | @tsv
      ' "$contract_file"
    )
  else
    echo "No hardware contract was produced. Preflight job outcome: **$(job_result "${PREFLIGHT_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Hardware matrix"
  echo
  if [[ -n "$matrix_file" && -s "$matrix_file" ]]; then
    echo "| Target | Workload | GPUs | Expected hosts | Topology | Outcome | Reason |"
    echo "|---|---|---:|---:|---|---|---|"
    while IFS=$'\t' read -r target workload workers hosts topology status reason; do
      printf '| %s | %s | %s | %s | %s | **%s** | %s |\n' \
        "$(markdown_cell "$target")" \
        "$(markdown_cell "$workload")" \
        "$(markdown_cell "$workers")" \
        "$(markdown_cell "$hosts")" \
        "$(markdown_cell "$topology")" \
        "$(markdown_cell "$status")" \
        "$(markdown_cell "$reason")"
    done < <(
      jq -r '
        [
          (.target // "all"),
          (.workload // "unknown"),
          (.workers // "-"),
          (.expected_hosts // "-"),
          (.required_topology // "-"),
          .status,
          (.reason // "")
        ]
        | @tsv
      ' "$matrix_file"
    )
  else
    echo "No matrix cases were recorded. Hardware matrix job outcome: **$(job_result "${MATRIX_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Cross-site"
  echo
  if [[ -n "$matrix_file" && -s "$matrix_file" ]] &&
    jq -e 'select(.workload == "cross-site")' "$matrix_file" >/dev/null; then
    while IFS=$'\t' read -r status reason; do
      printf -- '- Outcome: **%s**' "$(markdown_cell "$status")"
      if [[ -n "$reason" ]]; then
        printf ' - %s' "$(markdown_cell "$reason")"
      fi
      echo
    done < <(jq -r 'select(.workload == "cross-site") | [.status, (.reason // "")] | @tsv' "$matrix_file")
  else
    echo "- Outcome: **not recorded** - hardware matrix job $(job_result "${MATRIX_JOB_RESULT:-Unknown}")."
  fi
  echo

  echo "## Cleanup and postflight"
  echo
  if [[ -n "$postflight_file" ]]; then
    printf -- '- Namespace cleanup: **%s**' "$(jq -r '.cleanup.status' "$postflight_file")"
    cleanup_reason="$(jq -r '.cleanup.reason // ""' "$postflight_file")"
    [[ -z "$cleanup_reason" ]] || printf ' - %s' "$(markdown_cell "$cleanup_reason")"
    echo
    printf -- '- Capability restoration: **%s**' "$(jq -r '.capability.status' "$postflight_file")"
    capability_reason="$(jq -r '.capability.reason // ""' "$postflight_file")"
    [[ -z "$capability_reason" ]] || printf ' - %s' "$(markdown_cell "$capability_reason")"
    echo
  else
    echo "- Outcome unavailable; the hardware matrix job ended as **$(job_result "${MATRIX_JOB_RESULT:-Unknown}")** before postflight status was recorded."
  fi
  echo

  if [[ "${FLEX_NIGHTLY_RDMA_ENABLED:-false}" == "true" ||
    "${FLEX_NIGHTLY_RDMA_ENABLED:-false}" == "True" ]]; then
    echo "## Optional H200 RDMA"
    echo
    echo "- Job outcome: **$(job_result "${RDMA_JOB_RESULT:-Unknown}")**."
    echo
  fi

  echo "## Recorded E2E outcomes"
  echo
  e2e_file_list="$(find "$INPUT_DIR" -type f -name e2e-results.jsonl -print | LC_ALL=C sort)"
  if [[ -n "$e2e_file_list" ]]; then
    e2e_files=()
    while IFS= read -r e2e_file; do
      e2e_files+=("$e2e_file")
    done <<<"$e2e_file_list"
    jq -s -r '
      group_by(.status)
      | map("| \(.[0].status) | \(length) |")
      | .[]
    ' "${e2e_files[@]}" >"${OUTPUT_FILE}.e2e-counts"
    echo "| Status | Count |"
    echo "|---|---:|"
    cat "${OUTPUT_FILE}.e2e-counts"
    rm -f "${OUTPUT_FILE}.e2e-counts"
  else
    echo "No E2E result records were available."
  fi
} >"$OUTPUT_FILE"

echo "TauGrid Flex nightly report written to ${OUTPUT_FILE}"
