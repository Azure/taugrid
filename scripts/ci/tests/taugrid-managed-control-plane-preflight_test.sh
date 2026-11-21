#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly PREFLIGHT="${REPO_ROOT}/scripts/ci/taugrid-managed-control-plane-preflight.sh"

fail() {
  echo "TauGrid managed control-plane preflight test failed: $*" >&2
  exit 1
}

fake_bin="$(mktemp -d)"
trap 'rm -rf "$fake_bin"' EXIT

cat >"${fake_bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "$*" in
  *"get --raw /apis/kueue.x-k8s.io/v1beta2"*)
    if [[ "${FAKE_MISSING_KUEUE_RESOURCE:-0}" == "1" ]]; then
      printf '%s\n' '{"resources":[{"name":"workloads"},{"name":"clusterqueues"},{"name":"localqueues"},{"name":"resourceflavors"}]}'
    else
      printf '%s\n' '{"resources":[{"name":"workloads"},{"name":"clusterqueues"},{"name":"localqueues"},{"name":"resourceflavors"},{"name":"topologies"}]}'
    fi
    ;;
  *"get --raw /apis/ray.io/v1"*)
    printf '%s\n' '{"resources":[{"name":"rayjobs"},{"name":"rayclusters"},{"name":"rayservices"}]}'
    ;;
  *"--namespace kueue-system get deployment kueue-controller-manager --output json"*)
    printf '%s\n' '{"metadata":{"generation":2},"spec":{"replicas":1},"status":{"observedGeneration":2,"availableReplicas":1,"conditions":[{"type":"Available","status":"True"}]}}'
    ;;
  *"--namespace kuberay-system get deployment kuberay-operator --output json"*)
    if [[ "${FAKE_KUBERAY_AVAILABLE:-1}" == "1" ]]; then
      printf '%s\n' '{"metadata":{"generation":3},"spec":{"replicas":1},"status":{"observedGeneration":3,"availableReplicas":1,"conditions":[{"type":"Available","status":"True"}]}}'
    else
      printf '%s\n' '{"metadata":{"generation":3},"spec":{"replicas":1},"status":{"observedGeneration":3,"availableReplicas":0,"conditions":[{"type":"Available","status":"False"}]}}'
    fi
    ;;
  *)
    echo "unexpected kubectl invocation: $*" >&2
    exit 1
    ;;
esac
EOF
chmod +x "${fake_bin}/kubectl"

output="$(
  PATH="${fake_bin}:${PATH}" \
    KUBECTL_BIN="${fake_bin}/kubectl" \
    "$PREFLIGHT" unbounded-stable
)"
grep -Fq "Managed Kueue and KubeRay control planes are ready" <<<"$output" ||
  fail "ready managed control planes were not accepted"

missing_api_output="${fake_bin}/missing-api.txt"
if PATH="${fake_bin}:${PATH}" \
  KUBECTL_BIN="${fake_bin}/kubectl" \
  FAKE_MISSING_KUEUE_RESOURCE=1 \
  "$PREFLIGHT" unbounded-stable >"$missing_api_output" 2>&1; then
  fail "missing managed Kueue API was accepted"
fi
grep -Fq "does not serve required resource topologies" "$missing_api_output" ||
  fail "missing managed Kueue API error was not actionable"

unavailable_output="${fake_bin}/unavailable.txt"
if PATH="${fake_bin}:${PATH}" \
  KUBECTL_BIN="${fake_bin}/kubectl" \
  FAKE_KUBERAY_AVAILABLE=0 \
  "$PREFLIGHT" unbounded-stable >"$unavailable_output" 2>&1; then
  fail "unavailable managed KubeRay deployment was accepted"
fi
grep -Fq "Deployment kuberay-system/kuberay-operator is not available" "$unavailable_output" ||
  fail "unavailable managed KubeRay error was not actionable"

echo "TauGrid managed control-plane preflight tests passed"
