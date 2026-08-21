#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"${chart_dir}/scripts/sync-taucluster-catalog.sh" --check

rendered="$(helm template taucluster-default "${chart_dir}" --show-only templates/taucluster.yaml)"
if grep -Fq "executionTarget: multiKueue" <<<"${rendered}"; then
  echo "default TauCluster catalog must not contain a MultiKueue profile" >&2
  exit 1
fi

rbac="$(helm template controller-default "${chart_dir}" --show-only templates/rbac.yaml)"
for required in \
  'resources: ["admissionchecks"]' \
  'resources: ["multikueueconfigs", "multikueueclusters"]'; do
  if ! grep -Fq "$required" <<<"${rbac}"; then
    echo "controller RBAC is missing the unconditional MultiKueue prerequisite: ${required}" >&2
    exit 1
  fi
done
