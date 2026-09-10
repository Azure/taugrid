#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
temporary_directory="$(mktemp -d)"
trap 'rm -rf "$temporary_directory"' EXIT
helm_binary="$(command -v helm)"
failures=0
cases=0

while IFS= read -r test_case; do
  name="$(jq -r '.name' <<< "$test_case")"
  if [[ -n "${ADX_WAITER_CASE_FILTER:-}" && "$name" != *"$ADX_WAITER_CASE_FILTER"* ]]; then
    continue
  fi
  cases=$((cases + 1))
  release="$(jq -r '.release // "adx-mon"' <<< "$test_case")"
  namespace="$(jq -r '.namespace // "adx-mon"' <<< "$test_case")"
  jq '.values // {}' <<< "$test_case" > "$temporary_directory/values.json"
  "$helm_binary" template "$release" "$script_directory/../../charts/adx-mon" \
    --namespace "$namespace" --values "$script_directory/../../charts/adx-mon/values-ai-runtime.yaml" \
    --values "$temporary_directory/values.json" > "$temporary_directory/rendered.yaml"
  objects="$(yq eval-all -o=json -I=0 '[.]' "$temporary_directory/rendered.yaml" | jq -c \
    --arg release "$release" --arg namespace "$namespace" '
    [.[] | select(.kind == "Function") |
      .metadata += {uid: ("uid-" + .metadata.name), resourceVersion: "10", generation: 2,
        annotations: {"meta.helm.sh/release-name": $release, "meta.helm.sh/release-namespace": $namespace}} |
      .status = {observedGeneration: 2, status: "Success", error: ""}]')"
  if ! jq -e --argjson objects "$objects" '(.expectedCount // 9) == ($objects | length)' <<< "$test_case" >/dev/null; then
    echo "Unexpected real chart Function count for $name." >&2
    exit 1
  fi
  if jq -e '.duplicateRender' <<< "$test_case" >/dev/null; then
    printf '\n---\n%s\n' "$(jq '.[0]' <<< "$objects")" >> "$temporary_directory/rendered.yaml"
  fi
  printf '0' > "$temporary_directory/reads"
  printf '0' > "$temporary_directory/upgrades"
  printf 'preflight' > "$temporary_directory/phase"
  : > "$temporary_directory/deletes"
  : > "$temporary_directory/mock-errors"
  set +e
  (
    set -euo pipefail
    fail_if_requested() {
      if [[ "$(jq -r '.failure // ""' <<< "$test_case")" == "$1" ]]; then
        echo "mock $1 failure" >&2
        return 1
      fi
    }
    az() { fail_if_requested az; }
    helm() {
      if [[ "$1" == template ]]; then
        [[ "$*" == "template $release chart --namespace $namespace --values base-values --values environment-values" ]] ||
          { echo "Mismatched render arguments: $*" >> "$temporary_directory/mock-errors"; return 1; }
        fail_if_requested render || return 1
        if jq -e 'has("render")' <<< "$test_case" >/dev/null; then
          jq -r '.render' <<< "$test_case"
        else
          printf '%s\n' "$(<"$temporary_directory/rendered.yaml")"
        fi
      elif [[ "$1" == upgrade ]]; then
        [[ "$*" == "upgrade --install $release chart --namespace $namespace --values base-values --values environment-values --reset-values --kubeconfig kubeconfig --create-namespace --wait --timeout 30m" ]] ||
          { echo "Mismatched upgrade arguments: $*" >> "$temporary_directory/mock-errors"; return 1; }
        printf '%s' "$(($(<"$temporary_directory/upgrades") + 1))" > "$temporary_directory/upgrades"
        printf 'state' > "$temporary_directory/phase"
        fail_if_requested upgrade || return 1
      else
        echo "Unexpected helm command: $*" >&2
        return 97
      fi
    }
    kubectl() {
      if [[ "$1" == get ]]; then
        local index=0 filter response
        [[ "$*" == *"--kubeconfig kubeconfig --request-timeout=30s"* && "$*" == *"functions.adx-mon.azure.com"* ]] ||
          { echo "Unsafe read arguments: $*" >> "$temporary_directory/mock-errors"; return 1; }
        if [[ "$(<"$temporary_directory/phase")" == preflight ]]; then
          fail_if_requested preflight || return 1
          filter="$(jq -r '.preflight // "."' <<< "$test_case")"
          if (($(<"$temporary_directory/upgrades") > 0)); then
            filter="$(jq -r '.preflightAfterRetry // .preflight // "."' <<< "$test_case")"
          fi
        else
          index="$(<"$temporary_directory/reads")"
          printf '%s' "$((index + 1))" > "$temporary_directory/reads"
          fail_if_requested read || return 1
          if jq -e 'has("rawResponse")' <<< "$test_case" >/dev/null; then
            jq -r '.rawResponse' <<< "$test_case"
            return
          fi
          filter="$(jq -r --argjson index "$index" '.responses[([$index, (.responses | length) - 1] | min)]' <<< "$test_case")"
        fi
        response="$(jq -c "$filter" <<< "$objects")"
        printf '%s\n' "$response" > "$temporary_directory/last-objects"
        filter='.'
        if [[ "$(<"$temporary_directory/phase")" == state ]]; then filter="$(jq -r '.listFilter // "."' <<< "$test_case")"; fi
        jq -c '{apiVersion:"adx-mon.azure.com/v1", kind:"FunctionList", metadata:{}, items:.}' <<< "$response" | jq -c "$filter"
      elif [[ "$1" == delete ]]; then
        local body target
        body="$(jq -c .)"
        jq -cn --arg command "$*" --argjson body "$body" '{command:$command,body:$body}' >> "$temporary_directory/deletes"
        target="$(jq -c --arg path "${3:-}" '[.[] | select(("/apis/adx-mon.azure.com/v1/namespaces/" + .metadata.namespace + "/functions/" + .metadata.name) == $path)]' "$temporary_directory/last-objects")"
        if [[ "$*" != "delete --raw "*' --filename - --kubeconfig kubeconfig --request-timeout=30s' ]] ||
            ! jq -e --argjson body "$body" 'length == 1 and .[0].status.status == "PermanentFailure" and
              $body.kind == "DeleteOptions" and $body.apiVersion == "v1" and
              $body.preconditions == {uid:.[0].metadata.uid,resourceVersion:.[0].metadata.resourceVersion}' <<< "$target" >/dev/null; then
          echo "Invalid conditional-delete target/body: $* $body" >> "$temporary_directory/mock-errors"
          return 1
        fi
        fail_if_requested delete || return 1
        case "$(jq -r '.failure // ""' <<< "$test_case")" in
          conflict) echo 'Error from server (Conflict): precondition failed' >&2; return 1 ;;
          notfound) echo 'Error from server (NotFound): Function disappeared' >&2; return 1 ;;
        esac
      else
        echo "Unexpected kubectl command: $*" >&2
        return 97
      fi
    }
    sleep() {
      SECONDS=$((SECONDS + $1))
      if (($1 >= 60)); then printf 'preflight' > "$temporary_directory/phase"; fi
    }
    export ADX_FUNCTION_WAIT_SECONDS=60 MAXIMUM_ADX_FUNCTION_ATTEMPTS=6
    ADX_ALLOW_NO_FUNCTIONS="$(jq -r '.allowNoFunctions // false' <<< "$test_case")"
    export ADX_ALLOW_NO_FUNCTIONS
    set -- subscription resource-group cluster kubeconfig chart base-values environment-values "$release" "$namespace"
    source "$script_directory/wait-for-adx-functions-ready.sh"
  ) > "$temporary_directory/output" 2>&1
  result=$?
  set -e
  actual="$(jq -cn --argjson exit "$result" --argjson reads "$(<"$temporary_directory/reads")" \
    --argjson upgrades "$(<"$temporary_directory/upgrades")" \
    --argjson deletes "$(jq -s length "$temporary_directory/deletes")" \
    '{exit:$exit, reads:$reads, upgrades:$upgrades, deletes:$deletes}')"
  if ! jq -e --argjson actual "$actual" '.exit == $actual.exit and .reads == $actual.reads and .upgrades == $actual.upgrades and .deletes == $actual.deletes' <<< "$test_case" >/dev/null ||
      ! grep -F -- "$(jq -r '.diagnostic' <<< "$test_case")" "$temporary_directory/output" >/dev/null ||
      [[ -s "$temporary_directory/mock-errors" ]]; then
    echo "FAIL Bash: $name: $actual" >&2
    printf '%s\n' "$(<"$temporary_directory/output")" >&2
    printf '%s\n' "$(<"$temporary_directory/mock-errors")" >&2
    failures=$((failures + 1))
  else
    echo "PASS Bash: $name: $actual"
  fi
done < <(jq -c '.[]' "$script_directory/adx-function-waiter-cases.json")

echo "Bash: $cases cases, $failures failures."
[[ "$cases" -gt 0 && "$failures" -eq 0 ]]
python3 "$script_directory/test-adx-function-delete.py"
