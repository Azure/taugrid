#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT="${FLEX_NIGHTLY_KUBE_CONTEXT:-flex-nightly}"
readonly KUSTO_ENDPOINT="${FLEX_NIGHTLY_KUSTO_ENDPOINT:-}"
readonly KUSTO_RESOURCE="${FLEX_NIGHTLY_KUSTO_RESOURCE:-https://kusto.kusto.windows.net}"
readonly KUSTO_QUERY_DATABASE="${FLEX_NIGHTLY_KUSTO_QUERY_DATABASE:-Metrics}"
readonly KUSTO_RESULTS_DATABASE="${FLEX_NIGHTLY_KUSTO_RESULTS_DATABASE:-CITests}"
readonly PORTAL_NAMESPACE="${FLEX_NIGHTLY_PORTAL_NAMESPACE:-tau}"
readonly PORTAL_SERVICE="${FLEX_NIGHTLY_PORTAL_SERVICE:-taugrid-portal}"
readonly PORTAL_WORKSPACE="${FLEX_NIGHTLY_PORTAL_WORKSPACE:-default}"
readonly ARTIFACT_DIR="${FLEX_NIGHTLY_KUSTO_ARTIFACT_DIR:-}"
readonly BUILD_ID="${BUILD_BUILDID:-}"
readonly RESULT_FILE="${ARTIFACT_DIR}/kusto-check-result.json"

fail() {
  echo "TauGrid Flex Kusto check failed: $*" >&2
  exit 1
}

require_value() {
  local name="$1"
  local value="${!name:-}"
  [ -n "$value" ] || fail "${name} is required"
  [[ "$value" != '$('* ]] || fail "${name} is unresolved (${value})"
}

for name in KUSTO_ENDPOINT KUSTO_QUERY_DATABASE KUSTO_RESULTS_DATABASE \
  PORTAL_NAMESPACE PORTAL_SERVICE PORTAL_WORKSPACE ARTIFACT_DIR BUILD_ID; do
  require_value "$name"
done
command -v "$KUBECTL_BIN" >/dev/null 2>&1 || fail "${KUBECTL_BIN} is required"
command -v az >/dev/null 2>&1 || fail "az is required"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
command -v python3 >/dev/null 2>&1 || fail "python3 is required"

mkdir -p "$ARTIFACT_DIR"
portal_pid=""
cleanup() {
  local rc=$?
  trap - EXIT
  if [[ -n "$portal_pid" ]] && kill -0 "$portal_pid" >/dev/null 2>&1; then
    kill "$portal_pid"
    wait "$portal_pid" 2>/dev/null || true
  fi
  exit "$rc"
}
trap cleanup EXIT

port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
"$KUBECTL_BIN" --context "$KUBE_CONTEXT" -n "$PORTAL_NAMESPACE" \
  port-forward "service/${PORTAL_SERVICE}" "${port}:80" \
  >"${ARTIFACT_DIR}/portal-port-forward.log" 2>&1 &
portal_pid=$!
portal_base="http://127.0.0.1:${port}"

portal_ready=false
for _ in $(seq 1 30); do
  if curl --fail --silent --show-error "${portal_base}/healthz" \
    >"${ARTIFACT_DIR}/portal-health.json" 2>/dev/null &&
    jq -e '.status == "ok"' "${ARTIFACT_DIR}/portal-health.json" >/dev/null; then
    portal_ready=true
    break
  fi
  sleep 2
done
[[ "$portal_ready" == "true" ]] || fail "Portal health check did not become ready"

curl --fail --silent --show-error --get \
  "${portal_base}/api/v2/stellar/capabilities" \
  --data-urlencode "workspace=${PORTAL_WORKSPACE}" \
  >"${ARTIFACT_DIR}/portal-capabilities.json"
jq -e 'type == "object" and (.error? == null)' \
  "${ARTIFACT_DIR}/portal-capabilities.json" >/dev/null ||
  fail "Portal capabilities returned an error"

curl --fail --silent --show-error --get \
  "${portal_base}/api/v2/stellar/experiments" \
  --data-urlencode "workspace=${PORTAL_WORKSPACE}" \
  --data-urlencode "limit=1" \
  >"${ARTIFACT_DIR}/portal-experiments.json"
jq -e 'type == "object" and (.error? == null)' \
  "${ARTIFACT_DIR}/portal-experiments.json" >/dev/null ||
  fail "Portal experiment discovery returned an error"

token="$(az account get-access-token \
  --resource "$KUSTO_RESOURCE" \
  --query accessToken -o tsv)"
[[ -n "$token" ]] || fail "Azure CLI did not return a Kusto access token"
query="TestOutcomes | where RunID == tolong('${BUILD_ID}') | summarize rows=count(), failures=countif(Status !in~ ('pass','passed','skip','skipped'))"
payload="$(jq -cn \
  --arg db "$KUSTO_RESULTS_DATABASE" \
  --arg csl "$query" \
  '{db:$db,csl:$csl}')"

rows=0
failures=0
for _ in $(seq 1 30); do
  printf 'header = "Authorization: Bearer %s"
' "$token" |
    curl --config - \
      --fail --silent --show-error \
      -H "Content-Type: application/json" \
      --data "$payload" \
      "${KUSTO_ENDPOINT%/}/v1/rest/query" \
      >"${ARTIFACT_DIR}/test-outcomes-query.json"
  rows="$(jq -r '
    [.Tables[]? | select(.TableKind == "PrimaryResult") | .Rows[0][0]][0] // 0
  ' "${ARTIFACT_DIR}/test-outcomes-query.json")"
  failures="$(jq -r '
    [.Tables[]? | select(.TableKind == "PrimaryResult") | .Rows[0][1]][0] // 0
  ' "${ARTIFACT_DIR}/test-outcomes-query.json")"
  [[ "$rows" -gt 0 ]] && break
  sleep 20
done
unset token
[[ "$rows" -gt 0 ]] ||
  fail "No TestOutcomes rows were ingested for build ${BUILD_ID}"

jq -n \
  --arg status passed \
  --arg endpoint "$KUSTO_ENDPOINT" \
  --arg queryDatabase "$KUSTO_QUERY_DATABASE" \
  --arg resultsDatabase "$KUSTO_RESULTS_DATABASE" \
  --arg workspace "$PORTAL_WORKSPACE" \
  --argjson rows "$rows" \
  --argjson failures "$failures" \
  '{
    status:$status,
    endpoint:$endpoint,
    query_database:$queryDatabase,
    results_database:$resultsDatabase,
    portal_workspace:$workspace,
    test_outcomes:{rows:$rows,failures:$failures}
  }' >"$RESULT_FILE"

echo "TauGrid Flex Kusto checks passed with ${rows} TestOutcomes rows"
