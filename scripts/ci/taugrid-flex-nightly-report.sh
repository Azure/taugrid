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
gpu_class_inventory_file="$(find_first gpu-class-inventory.json)"
image_file="$(find_first images.json)"
deployment_file="$(find_first deployment-result.json)"
cli_results_file="$(find_first cli-smoke-results.jsonl)"
cli_cleanup_file="$(find_first cleanup-result.json)"
gpu_routing_results_file="$(find_first gpu-routing-results.jsonl)"
gpu_routing_cleanup_file="$(find_first gpu-routing-cleanup-result.json)"
gpu_profile_results_file="$(find_first gpu-profile-results.jsonl)"
gpu_profile_representatives_file="$(find_first gpu-profile-representatives.json)"
gpu_profile_cleanup_file="$(find_first gpu-profile-cleanup-result.json)"
storage_results_file="$(find_first storage-results.jsonl)"
storage_cleanup_file="$(find_first storage-cleanup-result.json)"
matrix_file="$(find_first matrix-results.jsonl)"
postflight_file="$(find_first postflight-status.json)"
rdma_postflight_file="$(find_first rdma-postflight-status.json)"
rdma_metrics_file="$(find_first fineweb-metrics.json)"
kusto_file="$(find_first kusto-check-result.json)"

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
  printf '| TauCluster reconciliation job | **%s** |\n' "$(job_result "${TAUCLUSTER_JOB_RESULT:-Unknown}")"
  printf '| Preflight job | **%s** |\n' "$(job_result "${PREFLIGHT_JOB_RESULT:-Unknown}")"
  printf '| Image publication job | **%s** |\n' "$(job_result "${BUILD_IMAGES_JOB_RESULT:-Unknown}")"
  printf '| Deployment job | **%s** |\n' "$(job_result "${DEPLOY_JOB_RESULT:-Unknown}")"
  printf '| Tau CLI smoke job | **%s** |\n' "$(job_result "${CLI_SMOKE_JOB_RESULT:-Unknown}")"
  printf '| GPU queue-routing job | **%s** |\n' "$(job_result "${GPU_ROUTING_JOB_RESULT:-Unknown}")"
  printf '| GPU profile job | **%s** |\n' "$(job_result "${GPU_PROFILE_JOB_RESULT:-Unknown}")"
  printf '| Cross-region storage job | **%s** |\n' "$(job_result "${STORAGE_JOB_RESULT:-Unknown}")"
  printf '| Hardware matrix job | **%s** |\n' "$(job_result "${MATRIX_JOB_RESULT:-Unknown}")"
  if [[ "${FLEX_NIGHTLY_RDMA_ENABLED:-false}" == "true" ||
    "${FLEX_NIGHTLY_RDMA_ENABLED:-false}" == "True" ]]; then
    printf '| H200 RDMA job | **%s** |\n' "$(job_result "${RDMA_JOB_RESULT:-Unknown}")"
  fi
  printf '| Portal and Kusto job | **%s** |\n' "$(job_result "${KUSTO_JOB_RESULT:-Unknown}")"
  echo

  echo "## TauCluster GPU discovery"
  echo
  if [[ -n "$gpu_class_inventory_file" ]]; then
    echo "| GPU class | Series | Node | Allocatable GPUs |"
    echo "|---|---|---|---:|"
    jq -r '
      .nodes[]
      | [.gpu_class, .gpu_series, .name, .gpu_allocatable]
      | @tsv
    ' "$gpu_class_inventory_file" |
      while IFS=$'\t' read -r gpu_class gpu_series node gpu; do
        printf '| `%s` | `%s` | `%s` | %s |\n' \
          "$(markdown_cell "$gpu_class")" \
          "$(markdown_cell "$gpu_series")" \
          "$(markdown_cell "$node")" \
          "$(markdown_cell "$gpu")"
      done
  else
    echo "- No GPU discovery inventory was produced. Reconciliation job outcome: **$(job_result "${TAUCLUSTER_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Tau GPU class queue routing"
  echo
  if [[ -n "$gpu_routing_results_file" && -s "$gpu_routing_results_file" ]]; then
    echo "| Target | Outcome | ResourceFlavor | Node | Reason |"
    echo "|---|---|---|---|---|"
    jq -r '[.target,.status,.admitted_flavor,.node,.reason] | @tsv' \
      "$gpu_routing_results_file" |
      while IFS=$'\t' read -r target status flavor node reason; do
        printf '| %s | **%s** | `%s` | `%s` | %s |\n' \
          "$(markdown_cell "$target")" \
          "$(markdown_cell "$status")" \
          "$(markdown_cell "$flavor")" \
          "$(markdown_cell "$node")" \
          "$(markdown_cell "$reason")"
      done
    if [[ -n "$gpu_routing_cleanup_file" ]]; then
      printf -- '- Cleanup: **%s**' \
        "$(markdown_cell "$(jq -r '.status // "unknown"' "$gpu_routing_cleanup_file")")"
      reason="$(jq -r '.reason // ""' "$gpu_routing_cleanup_file")"
      [[ -z "$reason" ]] || printf ' - %s' "$(markdown_cell "$reason")"
      echo
    fi
  else
    echo "- No GPU routing result was produced. Routing job outcome: **$(job_result "${GPU_ROUTING_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## TauGrid deployment"
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
    printf -- '- Helm revision: `%s` -> `%s`\n' \
      "$(markdown_cell "$(jq -r '.previous_revision // "unknown"' "$deployment_file")")" \
      "$(markdown_cell "$(jq -r '.current_revision // "unknown"' "$deployment_file")")"
  else
    echo "- No deployment result was produced. Deployment job outcome: **$(job_result "${DEPLOY_JOB_RESULT:-Unknown}")**."
  fi
  if [[ -n "$image_file" ]]; then
    printf -- '- Published source/tag: `%s` / `%s`\n' \
      "$(markdown_cell "$(jq -r '.source_version // "unknown"' "$image_file")")" \
      "$(markdown_cell "$(jq -r '.tag // "unknown"' "$image_file")")"
  else
    echo "- No image contract was produced. Image publication job outcome: **$(job_result "${BUILD_IMAGES_JOB_RESULT:-Unknown}")**."
  fi
  echo

  echo "## Tau GPU workload profiles"
  echo
  if [[ -n "$gpu_profile_results_file" && -s "$gpu_profile_results_file" ]]; then
    echo "| Profile | Phase | Shape | Outcome | Reason |"
    echo "|---|---|---|---|---|"
    while IFS=$'\t' read -r profile phase shape status reason; do
      printf '| %s | %s | %s | **%s** | %s |\n' \
        "$(markdown_cell "$profile")" \
        "$(markdown_cell "$phase")" \
        "$(markdown_cell "$shape")" \
        "$(markdown_cell "$status")" \
        "$(markdown_cell "$reason")"
    done < <(
      jq -r '[.profile, .phase, .shape, .status, (.reason // "")] | @tsv' \
        "$gpu_profile_results_file"
    )
  else
    echo "No GPU profile results were recorded. GPU profile job outcome: **$(job_result "${GPU_PROFILE_JOB_RESULT:-Unknown}")**."
  fi
  if [[ -n "$gpu_profile_representatives_file" ]]; then
    printf -- '- Unique live-tested shapes: **%s**\n' \
      "$(jq 'length' "$gpu_profile_representatives_file")"
  fi
  if [[ -n "$gpu_profile_cleanup_file" ]]; then
    printf -- '- GPU profile workload cleanup: **%s**' \
      "$(jq -r '.status' "$gpu_profile_cleanup_file")"
    gpu_profile_cleanup_reason="$(jq -r '.reason // ""' "$gpu_profile_cleanup_file")"
    [[ -z "$gpu_profile_cleanup_reason" ]] ||
      printf ' - %s' "$(markdown_cell "$gpu_profile_cleanup_reason")"
    echo
  else
    echo "- GPU profile cleanup outcome unavailable."
  fi
  echo

  echo "## Cross-region Azure Blob PVC"
  echo
  if [[ -n "$storage_results_file" && -s "$storage_results_file" ]]; then
    echo "| Check | Outcome | Reason |"
    echo "|---|---|---|"
    while IFS=$'\t' read -r target status reason; do
      printf '| %s | **%s** | %s |\n' \
        "$(markdown_cell "$target")" \
        "$(markdown_cell "$status")" \
        "$(markdown_cell "$reason")"
    done < <(jq -r '[.target, .status, (.reason // "")] | @tsv' "$storage_results_file")
  else
    echo "No storage cases were recorded. Storage job outcome: **$(job_result "${STORAGE_JOB_RESULT:-Unknown}")**."
  fi
  if [[ -n "$storage_cleanup_file" ]]; then
    printf -- '- Blob test data and Job cleanup: **%s**' \
      "$(jq -r '.status' "$storage_cleanup_file")"
    storage_cleanup_reason="$(jq -r '.reason // ""' "$storage_cleanup_file")"
    [[ -z "$storage_cleanup_reason" ]] ||
      printf ' - %s' "$(markdown_cell "$storage_cleanup_reason")"
    echo
  else
    echo "- Storage cleanup outcome unavailable."
  fi
  echo

  echo "## Tau CLI functional smoke"
  echo
  if [[ -n "$cli_results_file" && -s "$cli_results_file" ]]; then
    echo "| Command | Outcome | Duration | Reason |"
    echo "|---|---|---:|---|"
    while IFS=$'\t' read -r command status duration reason; do
      printf '| %s | **%s** | %ss | %s |\n' \
        "$(markdown_cell "$command")" \
        "$(markdown_cell "$status")" \
        "$(markdown_cell "$duration")" \
        "$(markdown_cell "$reason")"
    done < <(
      jq -r '[.command, .status, (.duration_seconds // 0), (.reason // "")] | @tsv' \
        "$cli_results_file"
    )
  else
    echo "No Tau CLI command results were recorded. CLI smoke job outcome: **$(job_result "${CLI_SMOKE_JOB_RESULT:-Unknown}")**."
  fi
  if [[ -n "$cli_cleanup_file" ]]; then
    printf -- '- Workload cleanup: **%s**' "$(jq -r '.status' "$cli_cleanup_file")"
    cli_cleanup_reason="$(jq -r '.reason // ""' "$cli_cleanup_file")"
    [[ -z "$cli_cleanup_reason" ]] || printf ' - %s' "$(markdown_cell "$cli_cleanup_reason")"
    echo
  else
    echo "- Workload cleanup outcome unavailable."
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
    echo "## H200 InfiniBand and NCCL"
    echo
    echo "- Job outcome: **$(job_result "${RDMA_JOB_RESULT:-Unknown}")**."
    if [[ -n "$rdma_postflight_file" ]]; then
      printf -- '- Namespace cleanup: **%s**' "$(jq -r '.cleanup.status' "$rdma_postflight_file")"
      rdma_cleanup_reason="$(jq -r '.cleanup.reason // ""' "$rdma_postflight_file")"
      [[ -z "$rdma_cleanup_reason" ]] || printf ' - %s' "$(markdown_cell "$rdma_cleanup_reason")"
      echo
      printf -- '- Capability restoration: **%s**' "$(jq -r '.capability.status' "$rdma_postflight_file")"
      rdma_capability_reason="$(jq -r '.capability.reason // ""' "$rdma_postflight_file")"
      [[ -z "$rdma_capability_reason" ]] || printf ' - %s' "$(markdown_cell "$rdma_capability_reason")"
      echo
    else
      echo "- Cleanup outcome unavailable; the RDMA job ended as **$(job_result "${RDMA_JOB_RESULT:-Unknown}")** before postflight status was recorded."
    fi
    if [[ -n "$rdma_metrics_file" ]]; then
      echo
      echo "| InfiniBand metric | Value |"
      echo "|---|---:|"
      printf '| Sampled H200 hosts | %s |\n' \
        "$(jq -r '.infiniband.hosts' "$rdma_metrics_file")"
      printf '| Training throughput | %s tokens/s |\n' \
        "$(jq -r '.performance.tokens_per_second | floor' "$rdma_metrics_file")"
      printf '| Measured duration | %ss |\n' \
        "$(jq -r '.performance.duration_seconds' "$rdma_metrics_file")"
      printf '| InfiniBand transmit | %s bytes / %s packets |\n' \
        "$(jq -r '.infiniband.transmit_bytes' "$rdma_metrics_file")" \
        "$(jq -r '.infiniband.transmit_packets' "$rdma_metrics_file")"
      printf '| InfiniBand receive | %s bytes / %s packets |\n' \
        "$(jq -r '.infiniband.receive_bytes' "$rdma_metrics_file")" \
        "$(jq -r '.infiniband.receive_packets' "$rdma_metrics_file")"
      printf '| InfiniBand error events | %s |\n' \
        "$(jq -r '.infiniband.error_events' "$rdma_metrics_file")"
    else
      echo "- InfiniBand metrics unavailable."
    fi
    echo
  fi

  echo "## Portal and Kusto"
  echo
  if [[ -n "$kusto_file" ]]; then
    printf -- '- Portal workspace: `%s`\n' \
      "$(markdown_cell "$(jq -r '.portal_workspace // "unknown"' "$kusto_file")")"
    printf -- '- Query database: `%s`\n' \
      "$(markdown_cell "$(jq -r '.query_database // "unknown"' "$kusto_file")")"
    printf -- '- Results database: `%s`\n' \
      "$(markdown_cell "$(jq -r '.results_database // "unknown"' "$kusto_file")")"
    printf -- '- Ingested TestOutcomes rows: **%s** (%s recorded test failures)\n' \
      "$(jq -r '.test_outcomes.rows // 0' "$kusto_file")" \
      "$(jq -r '.test_outcomes.failures // 0' "$kusto_file")"
  else
    echo "No Portal/Kusto result was produced. Kusto job outcome: **$(job_result "${KUSTO_JOB_RESULT:-Unknown}")**."
  fi
  echo

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
