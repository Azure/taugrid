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
maximum_attempts="${MAXIMUM_ADX_FUNCTION_ATTEMPTS:-6}"
function_wait_seconds="${ADX_FUNCTION_WAIT_SECONDS:-300}"

az aks get-credentials \
  --admin \
  --subscription "$subscription_id" \
  --resource-group "$resource_group" \
  --name "$cluster_name" \
  --file "$kubeconfig" \
  --overwrite-existing

for ((attempt = 1; attempt <= maximum_attempts; attempt++)); do
  helm upgrade --install adx-mon "$chart_path" \
    --namespace adx-mon \
    --create-namespace \
    --values "$base_values_file" \
    --values "$environment_values_file" \
    --wait \
    --timeout 30m

  deadline=$((SECONDS + function_wait_seconds))
  permanent_failure=""
  while ((SECONDS < deadline)); do
    function_statuses="$(kubectl get functions --namespace adx-mon --output=jsonpath='{range .items[*]}{.metadata.name}{"\\t"}{.status.status}{"\\n"}{end}')"
    if [[ -n "$function_statuses" ]]; then
      permanent_failure="$(printf '%s\n' "$function_statuses" | awk -F '\t' '$2 == "PermanentFailure" { print $1 }')"
      if [[ -n "$permanent_failure" ]]; then
        break
      fi
      if ! printf '%s\n' "$function_statuses" | awk -F '\t' '$2 != "Success" { exit 1 }'; then
        echo "All adx-mon Functions reached Success."
        exit 0
      fi
    fi
    sleep 15
  done

  if [[ -n "$permanent_failure" ]]; then
    echo "adx-mon Functions reached PermanentFailure on attempt $attempt: $permanent_failure" >&2
  else
    echo "adx-mon Functions did not reach Success within $function_wait_seconds seconds on attempt $attempt." >&2
  fi
  if ((attempt == maximum_attempts)); then
    echo "adx-mon Functions did not reach Success after $maximum_attempts attempts." >&2
    exit 1
  fi

  kubectl delete functions --namespace adx-mon --all --ignore-not-found
  sleep $((60 * attempt))
done
