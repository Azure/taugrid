#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
delete_log="$(mktemp)"
trap 'rm -f "$delete_log"' EXIT

run_waiter() {
  local -a function_statuses=("$@")
  local stage_file
  local expected_jsonpath='--output=jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.generation}{"\t"}{.status.observedGeneration}{"\t"}{.status.status}{"\t"}{.status.error}{"\n"}{end}'
  stage_file="$(mktemp)"
  printf '0' > "$stage_file"

  (
    az() { :; }
    helm() { :; }
    kubectl() {
      if [[ "$1" == "get" ]]; then
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
        printf '%s\n' "${function_statuses[$stage]}"
        printf '%s' "$((stage + 1))" > "$stage_file"
      elif [[ "$1" == "delete" ]]; then
        printf '%s\n' "$*" >> "$delete_log"
      fi
    }
    sleep() { SECONDS=$deadline; }

    export ADX_FUNCTION_WAIT_SECONDS=1
    export MAXIMUM_ADX_FUNCTION_ATTEMPTS=2
    set -- subscription resource-group cluster kubeconfig chart base-values environment-values
    source "$script_directory/wait-for-adx-functions-ready.sh"
  )
  local status=$?
  rm -f "$stage_file"
  return "$status"
}

run_waiter $'metrics\t2\t2\tSuccess\t\nlogs\t4\t4\tSuccess\t'

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
