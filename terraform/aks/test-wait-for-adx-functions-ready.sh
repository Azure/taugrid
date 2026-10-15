#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
delete_log="$(mktemp)"
trap 'rm -f "$delete_log"' EXIT

rendered_values="$(
  terraform -chdir="$script_directory" console -no-color <<'EOF'
templatefile("adx-mon-values.yaml.tftpl", {adx_endpoint="https://test.kusto.windows.net", adx_client_id="client-id", cluster_name="cluster", location="eastus", gpu_node_pool_name="gpu", gpu_stack_mode="self_managed", typed_experiment_telemetry_enabled=true})
EOF
)"
for expected in \
  "typedMetricEventsV1:" \
  "experimentCatalogV1:" \
  "tauExpMetricEventRows:" \
  "tauExpSeriesCatalogRows:" \
  "tauExpRunCatalogRows:"; do
  if ! grep -F "$expected" <<<"$rendered_values" >/dev/null; then
    echo "The complete Terraform ADX values omitted $expected" >&2
    exit 1
  fi
done

run_waiter() {
  local -a function_statuses=("$@")
  local stage_file
  local management_stage_file
  local expected_jsonpath='--output=jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.generation}{"\t"}{.status.observedGeneration}{"\t"}{.status.status}{"\t"}{.status.error}{"\n"}{end}'
  stage_file="$(mktemp)"
  management_stage_file="$(mktemp)"
  printf '0' > "$stage_file"
  printf '0' > "$management_stage_file"

  (
    az() { :; }
    helm() { :; }
    kubectl() {
      if [[ "$1" == "get" ]]; then
        if [[ "$2" == "functions" ]]; then
          local has_expected_jsonpath=false
          local argument
          for argument in "$@"; do
            if [[ "$argument" == "$expected_jsonpath" ]]; then
              has_expected_jsonpath=true
              break
            fi
          done
          if [[ "$has_expected_jsonpath" != true ]]; then
            echo "The waiter did not request the expected Function JSONPath." >&2
            return 1
          fi
          stage="$(<"$stage_file")"
          if ((stage >= ${#function_statuses[@]})); then
            stage=$((${#function_statuses[@]} - 1))
          fi
          printf '%s\n' "${function_statuses[$stage]}"
          printf '%s' "$((stage + 1))" > "$stage_file"
        elif [[ "$2" == "managementcommands" ]]; then
          if declare -p MANAGEMENT_COMMAND_STATUS_RESPONSES >/dev/null 2>&1; then
            stage="$(<"$management_stage_file")"
            if ((stage >= ${#MANAGEMENT_COMMAND_STATUS_RESPONSES[@]})); then
              stage=$((${#MANAGEMENT_COMMAND_STATUS_RESPONSES[@]} - 1))
            fi
            printf '%s\n' "${MANAGEMENT_COMMAND_STATUS_RESPONSES[$stage]}"
            printf '%s' "$((stage + 1))" > "$management_stage_file"
          else
            printf '%s\n' "${MANAGEMENT_COMMAND_STATUSES:-}"
          fi
        elif [[ "$2" == "managementcommand" && "$3" == "taugrid-lifecycle-schema" ]]; then
          printf '%s\n' "${LIFECYCLE_SCHEMA_STATUS:-}"
        fi
      elif [[ "$1" == "delete" ]]; then
        printf '%s\n' "$*" >> "$delete_log"
      fi
    }
    sleep() {
      if [[ "${ALLOW_PENDING_REPOLL:-false}" != "true" ]]; then
        SECONDS=$deadline
      fi
    }

    export ADX_FUNCTION_WAIT_SECONDS=1
    export MAXIMUM_ADX_FUNCTION_ATTEMPTS=2
    set -- subscription resource-group cluster kubeconfig chart base-values environment-values "${TYPED_EXPERIMENT_TELEMETRY_ENABLED:-false}"
    source "$script_directory/wait-for-adx-functions-ready.sh"
  )
  local status=$?
  rm -f "$stage_file" "$management_stage_file"
  return "$status"
}

run_waiter $'metrics\t2\t2\tSuccess\t\nlogs\t4\t4\tSuccess\t'

TYPED_EXPERIMENT_TELEMETRY_ENABLED=true
LIFECYCLE_SCHEMA_STATUS=$'1\t1\tTrue\t'
MANAGEMENT_COMMAND_STATUSES=$'adx-mon-typed-metric-events-v1\t1\t1\tTrue\t\nadx-mon-typed-metric-events-v1-retention\t1\t1\tTrue\t\nadx-mon-experiment-catalog-v1\t1\t1\tTrue\t\nadx-mon-experiment-catalog-v1-retention\t1\t1\tTrue\t'
run_waiter $'adx-mon-tau-exp-metric-event-rows\t1\t1\tSuccess\t\nadx-mon-tau-exp-series-catalog-rows\t1\t1\tSuccess\t\nadx-mon-tau-exp-run-catalog-rows\t1\t1\tSuccess\t'
MANAGEMENT_COMMAND_STATUSES=""
if run_waiter $'metrics\t2\t2\tSuccess\t' >/dev/null 2>&1; then
  echo "The waiter accepted missing typed experiment resources." >&2
  exit 1
fi
unset TYPED_EXPERIMENT_TELEMETRY_ENABLED LIFECYCLE_SCHEMA_STATUS MANAGEMENT_COMMAND_STATUSES

if run_waiter $'metrics\t2\t1\tSuccess\t' >/dev/null 2>&1; then
  echo "The waiter accepted stale Function status." >&2
  exit 1
fi

if run_waiter $'metrics\t2\t2\tPermanentFailure\tinvalid KQL' >/dev/null 2>&1; then
  echo "The waiter accepted a terminal Function failure." >&2
  exit 1
fi

run_waiter \
  $'metrics\t2\t2\tPermanentFailure\tRequestRateLimitPolicy throttling' \
  $'metrics\t3\t3\tSuccess\t'

if ! grep -Fx 'delete functions --namespace adx-mon metrics --ignore-not-found' "$delete_log" >/dev/null; then
  echo "The waiter did not delete only the retryable Function." >&2
  exit 1
fi

TYPED_EXPERIMENT_TELEMETRY_ENABLED=true
LIFECYCLE_SCHEMA_STATUS=$'1\t1\tTrue\t'
MANAGEMENT_COMMAND_STATUSES=$'adx-mon-experiment-catalog-v1\t1\t1\tFalse\tinvalid KQL'
if run_waiter $'adx-mon-tau-exp-metric-event-rows\t1\t1\tSuccess\t\nadx-mon-tau-exp-series-catalog-rows\t1\t1\tSuccess\t\nadx-mon-tau-exp-run-catalog-rows\t1\t1\tSuccess\t' >/dev/null 2>&1; then
  echo "The waiter accepted a terminal ManagementCommand failure." >&2
  exit 1
fi

ALLOW_PENDING_REPOLL=true
MANAGEMENT_COMMAND_STATUS_RESPONSES=(
  $'adx-mon-typed-metric-events-v1\t1\t1\tTrue\t\nadx-mon-typed-metric-events-v1-retention\t1\t1\tFalse\tMaterialized view TauExpMetricEventsV1Dedup was not found\nadx-mon-experiment-catalog-v1\t1\t1\tFalse\tFailed to resolve table expression named TauExpMetricEventsV1\nadx-mon-experiment-catalog-v1-retention\t1\t1\tFalse\tMaterialized view TauExpTypedSeriesCatalogV1 does not exist'
  $'adx-mon-typed-metric-events-v1\t1\t1\tTrue\t\nadx-mon-typed-metric-events-v1-retention\t1\t1\tTrue\t\nadx-mon-experiment-catalog-v1\t1\t1\tTrue\t\nadx-mon-experiment-catalog-v1-retention\t1\t1\tTrue\t'
)
run_waiter $'adx-mon-tau-exp-metric-event-rows\t1\t1\tSuccess\t\nadx-mon-tau-exp-series-catalog-rows\t1\t1\tSuccess\t\nadx-mon-tau-exp-run-catalog-rows\t1\t1\tSuccess\t'
unset ALLOW_PENDING_REPOLL MANAGEMENT_COMMAND_STATUS_RESPONSES
