#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

subscription_id="$1"
resource_group="$2"
cluster_name="$3"
kubeconfig="$4"
chart_path="$5"
base_values_file="$6"
environment_values_file="$7"
typed_experiment_telemetry_enabled="${8:-false}"
maximum_attempts="${MAXIMUM_ADX_FUNCTION_ATTEMPTS:-6}"
function_wait_seconds="${ADX_FUNCTION_WAIT_SECONDS:-300}"
required_management_command_pattern='^adx-mon-(typed-metric-events-v1|typed-metric-events-v1-retention|experiment-catalog-v1|experiment-catalog-v1-retention)$'
required_function_pattern='^adx-mon-tau-exp-(metric-event-rows|series-catalog-rows|run-catalog-rows)$'

az aks get-credentials \
  --admin \
  --subscription "$subscription_id" \
  --resource-group "$resource_group" \
  --name "$cluster_name" \
  --file "$kubeconfig" \
  --overwrite-existing

if [[ "$typed_experiment_telemetry_enabled" == "true" ]]; then
  lifecycle_deadline=$((SECONDS + function_wait_seconds))
  while ((SECONDS < lifecycle_deadline)); do
    lifecycle_status="$(
      kubectl get managementcommand taugrid-lifecycle-schema --namespace adx-mon \
        --output=jsonpath='{.metadata.generation}{"\t"}{.status.conditions[0].observedGeneration}{"\t"}{.status.conditions[0].status}{"\t"}{.status.conditions[0].message}'
    )"
    if awk -F '\t' '$1 == $2 && $3 == "False" { exit 0 } { exit 1 }' <<<"$lifecycle_status"; then
      echo "TauExpRunLifecycle schema reconciliation failed: $lifecycle_status" >&2
      exit 1
    fi
    if awk -F '\t' '$1 == $2 && $3 == "True" { exit 0 } { exit 1 }' <<<"$lifecycle_status"; then
      break
    fi
    sleep 15
  done
  if ! awk -F '\t' '$1 == $2 && $3 == "True" { exit 0 } { exit 1 }' <<<"${lifecycle_status:-}"; then
    echo "TauExpRunLifecycle schema did not reach Success within $function_wait_seconds seconds." >&2
    exit 1
  fi
fi

for ((attempt = 1; attempt <= maximum_attempts; attempt++)); do
  helm upgrade --install adx-mon "$chart_path" \
    --namespace adx-mon \
    --create-namespace \
    --values "$base_values_file" \
    --values "$environment_values_file" \
    --wait \
    --timeout 30m

  deadline=$((SECONDS + function_wait_seconds))
  retryable_function_names=()
  retryable_management_command_names=()
  while ((SECONDS < deadline)); do
    function_statuses="$(kubectl get functions --namespace adx-mon --output=jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.generation}{"\t"}{.status.observedGeneration}{"\t"}{.status.status}{"\t"}{.status.error}{"\n"}{end}')"
    management_command_statuses="$(
      kubectl get managementcommands --namespace adx-mon \
        --output=jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.generation}{"\t"}{.status.conditions[0].observedGeneration}{"\t"}{.status.conditions[0].status}{"\t"}{.status.conditions[0].message}{"\n"}{end}' |
        awk -F '\t' -v pattern="$required_management_command_pattern" '$1 ~ pattern'
    )"
    if [[ -n "$function_statuses" ]]; then
      terminal_failure="$(printf '%s\n' "$function_statuses" | awk -F '\t' '
        $2 == $3 && $4 == "PermanentFailure" && $5 !~ /[Tt][Hh][Rr][Oo][Tt][Tt][Ll]|[Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Aa][Tt][Ee][Ll][Ii][Mm][Ii][Tt][Pp][Oo][Ll][Ii][Cc][Yy]|[Tt][Oo][Oo][Mm][Aa][Nn][Yy][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Ss]/ {
          print $1 " (generation=" $2 ", observedGeneration=" $3 ", status=" $4 ", error=" $5 ")"
        }')"
      if [[ -n "$terminal_failure" ]]; then
        echo "adx-mon Function reconciliation reached a terminal failure: $terminal_failure" >&2
        exit 1
      fi
      mapfile -t retryable_function_names < <(printf '%s\n' "$function_statuses" | awk -F '\t' '
        $2 == $3 && $4 == "PermanentFailure" && $5 ~ /[Tt][Hh][Rr][Oo][Tt][Tt][Ll]|[Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Aa][Tt][Ee][Ll][Ii][Mm][Ii][Tt][Pp][Oo][Ll][Ii][Cc][Yy]|[Tt][Oo][Oo][Mm][Aa][Nn][Yy][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Ss]/ { print $1 }')
    fi

    if [[ -n "$management_command_statuses" ]]; then
      management_command_terminal_failure="$(printf '%s\n' "$management_command_statuses" | awk -F '\t' '
        $2 == $3 && $4 == "False" &&
          $5 !~ /[Tt][Hh][Rr][Oo][Tt][Tt][Ll]|[Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Aa][Tt][Ee][Ll][Ii][Mm][Ii][Tt][Pp][Oo][Ll][Ii][Cc][Yy]|[Tt][Oo][Oo][Mm][Aa][Nn][Yy][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Ss]/ &&
          $5 !~ /[Ff]ailed to resolve (table|materialized-view)|[Tt]able .* (does not exist|was not found)|[Mm]aterialized[- ]view .* (does not exist|was not found)/ {
          print $1 " (generation=" $2 ", observedGeneration=" $3 ", status=" $4 ", error=" $5 ")"
        }')"
      if [[ -n "$management_command_terminal_failure" ]]; then
        echo "adx-mon ManagementCommand reconciliation reached a terminal failure: $management_command_terminal_failure" >&2
        exit 1
      fi
      mapfile -t retryable_management_command_names < <(printf '%s\n' "$management_command_statuses" | awk -F '\t' '
        $2 == $3 && $4 == "False" && $5 ~ /[Tt][Hh][Rr][Oo][Tt][Tt][Ll]|[Rr][Ee][Qq][Uu][Ee][Ss][Tt][Rr][Aa][Tt][Ee][Ll][Ii][Mm][Ii][Tt][Pp][Oo][Ll][Ii][Cc][Yy]|[Tt][Oo][Oo][Mm][Aa][Nn][Yy][Rr][Ee][Qq][Uu][Ee][Ss][Tt][Ss]/ { print $1 }')
    fi

    if ((${#retryable_function_names[@]} > 0 || ${#retryable_management_command_names[@]} > 0)); then
      break
    fi

    functions_ready=false
    if [[ -n "$function_statuses" ]] &&
      printf '%s\n' "$function_statuses" | awk -F '\t' '$2 != $3 || $4 != "Success" { exit 1 }'; then
      functions_ready=true
    fi
    if [[ "$typed_experiment_telemetry_enabled" == "true" ]]; then
      typed_function_count="$(
        printf '%s\n' "$function_statuses" |
          awk -F '\t' -v pattern="$required_function_pattern" '$1 ~ pattern { count++ } END { print count + 0 }'
      )"
      if ((typed_function_count != 3)); then
        functions_ready=false
      fi
    fi
    management_commands_ready=false
    management_command_count="$(printf '%s\n' "$management_command_statuses" | awk 'NF { count++ } END { print count + 0 }')"
    if [[ "$typed_experiment_telemetry_enabled" != "true" ]] && ((management_command_count == 0)); then
      management_commands_ready=true
    elif ((management_command_count == 4)) &&
      printf '%s\n' "$management_command_statuses" | awk -F '\t' '$2 != $3 || $4 != "True" { exit 1 }'; then
      management_commands_ready=true
    fi
    if [[ "$functions_ready" == true && "$management_commands_ready" == true ]]; then
      echo "All adx-mon Functions and typed experiment ManagementCommands reached Success."
      exit 0
    fi
    sleep 15
  done

  if ((${#retryable_function_names[@]} == 0 && ${#retryable_management_command_names[@]} == 0)); then
    echo "adx-mon Functions and typed experiment ManagementCommands did not reach Success within $function_wait_seconds seconds on attempt $attempt." >&2
    exit 1
  fi
  echo "adx-mon resources reached retryable ADX throttling on attempt $attempt: functions=${retryable_function_names[*]:-none}; managementCommands=${retryable_management_command_names[*]:-none}" >&2
  if ((attempt == maximum_attempts)); then
    echo "adx-mon resources did not recover from ADX throttling after $maximum_attempts attempts." >&2
    exit 1
  fi

  if ((${#retryable_function_names[@]} > 0)); then
    kubectl delete functions --namespace adx-mon "${retryable_function_names[@]}" --ignore-not-found
  fi
  if ((${#retryable_management_command_names[@]} > 0)); then
    kubectl delete managementcommands --namespace adx-mon "${retryable_management_command_names[@]}" --ignore-not-found
  fi
  sleep $((60 * attempt))
done
