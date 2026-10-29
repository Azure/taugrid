#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly CHART_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

manifest="${TEST_ROOT}/manifest.yaml"
helm template taugrid-kueue "$CHART_DIR" \
  --namespace kueue-system \
  --include-crds >"$manifest"

for required in \
  'name: multikueueclusters.kueue.x-k8s.io' \
  'name: multikueueconfigs.kueue.x-k8s.io' \
  '--feature-gates=MultiKueue=true'; do
  grep -Fq -- "$required" "$manifest" || {
    echo "missing Kueue capability: $required" >&2
    exit 1
  }
done

retained_crd_count="$(grep -c 'helm.sh/resource-policy: keep' "$manifest")"
[[ "$retained_crd_count" -eq 11 ]] || {
  echo "expected all 11 Kueue CRDs to retain the Helm keep policy, got $retained_crd_count" >&2
  exit 1
}

if grep -Eq '^kind: (AdmissionCheck|MultiKueueConfig|MultiKueueCluster)$' "$manifest"; then
  echo "self-managed chart created operator-owned MultiKueue routing objects" >&2
  exit 1
fi

if helm template taugrid-kueue "$CHART_DIR" \
  --namespace tau-system >"${TEST_ROOT}/invalid.yaml" 2>"${TEST_ROOT}/invalid.err"; then
  echo "Kueue chart rendered outside kueue-system" >&2
  exit 1
fi
grep -Fq "must be installed into the 'kueue-system' namespace" "${TEST_ROOT}/invalid.err"

echo "TauGrid Kueue chart script tests passed"
