#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TEST_CHART_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_REPO_ROOT="$(cd -- "${TEST_CHART_DIR}/../.." && pwd)"
readonly TEST_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
  echo "assertion failed: $*" >&2
  exit 1
}

mkdir -p "$TEST_ROOT/bin" "$TEST_ROOT/source-probe/charts"
printf 'source guard marker\n' >"$TEST_ROOT/source-probe/charts/marker"
cat >"$TEST_ROOT/bin/helm" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$HELM_CALL_LOG"
exit 99
EOF
chmod +x "$TEST_ROOT/bin/helm"

original_path="$PATH"
export HELM_CALL_LOG="$TEST_ROOT/helm-calls"
export PATH="$TEST_ROOT/bin:$PATH"
# shellcheck source=../../../scripts/ci/vendor-taugrid-dependencies.sh
source "${TEST_REPO_ROOT}/scripts/ci/vendor-taugrid-dependencies.sh" "$TEST_ROOT/source-probe"
export PATH="$original_path"

[[ -f "$TEST_ROOT/source-probe/charts/marker" ]] ||
  fail "sourcing the vendoring helper mutated the chart directory"
[[ ! -e "$HELM_CALL_LOG" ]] ||
  fail "sourcing the vendoring helper executed Helm"

assert_older() {
  local candidate="$1"
  local reference="$2"

  version_is_older "$candidate" "$reference" ||
    fail "expected $candidate to have lower precedence than $reference"
}

assert_not_older() {
  local candidate="$1"
  local reference="$2"
  local status

  if version_is_older "$candidate" "$reference"; then
    fail "expected $candidate not to have lower precedence than $reference"
  else
    status=$?
  fi
  [[ "$status" -eq 1 ]] || fail "expected valid semantic versions: $candidate and $reference"
}

assert_invalid() {
  local candidate="$1"
  local status

  if version_is_older "$candidate" "1.0.0" >/dev/null 2>&1; then
    fail "expected invalid semantic version to be rejected: $candidate"
  else
    status=$?
  fi
  [[ "$status" -eq 2 ]] || fail "expected status 2 for invalid semantic version $candidate, got $status"
}

assert_equivalent() {
  assert_not_older "$1" "$2"
  assert_not_older "$2" "$1"
}

assert_older "0.1.3" "0.1.4"
assert_not_older "0.1.4" "0.1.4"
assert_not_older "0.2.0" "0.1.4"
assert_older "999999999999999999999.0.0" "1000000000000000000000.0.0"
assert_not_older "1000000000000000000000.0.0" "999999999999999999999.0.0"

assert_older "0.1.4-rc" "0.1.4"
assert_older "0.1.4-rc.1" "0.1.4-rc.2"
assert_older "0.1.4-999999999999999999999" "0.1.4-1000000000000000000000"
assert_older "0.1.4-1" "0.1.4-rc"
assert_older "0.1.4-rc" "0.1.4-rc.1"
assert_older "1.0.0-alpha" "1.0.0-alpha.1"
assert_older "1.0.0-alpha.1" "1.0.0-alpha.beta"
assert_older "1.0.0-alpha.beta" "1.0.0-beta"
assert_older "1.0.0-beta" "1.0.0-beta.2"
assert_older "1.0.0-beta.2" "1.0.0-beta.11"
assert_older "1.0.0-beta.11" "1.0.0-rc.1"
assert_older "1.0.0-rc.1" "1.0.0"
assert_equivalent "0.1.4+build.2" "0.1.4+build.1"
assert_equivalent "0.1.4-rc.1+build.2" "0.1.4-rc.1+build.1"

assert_invalid "0.1"
assert_invalid "01.1.0"
assert_invalid "0.1.4-rc.01"
assert_invalid "0.1.4-"
assert_invalid "0.1.4+"
assert_invalid "0.1.4-rc..1"
assert_invalid "0.1.4-rc."
assert_invalid "0.1.4+build."
assert_invalid "0.1.4+build..1"

render_namespaces() {
  awk '
    BEGIN { RS = "---"; FS = "\n" }
    function clean(value) {
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
      if ((substr(value, 1, 1) == "\"" && substr(value, length(value), 1) == "\"") ||
          (substr(value, 1, 1) == "'\''" && substr(value, length(value), 1) == "'\''")) {
        value = substr(value, 2, length(value) - 2)
      }
      return value
    }
    {
      kind = ""
      name = ""
      namespace = ""
      in_metadata = 0
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^kind: /) {
          kind = clean(substr($i, 7))
        } else if ($i == "metadata:") {
          in_metadata = 1
        } else if (in_metadata && $i ~ /^[^ ]/) {
          in_metadata = 0
        } else if (in_metadata && name == "" && $i ~ /^  name: /) {
          name = clean(substr($i, 9))
        } else if (in_metadata && $i ~ /^  namespace: /) {
          namespace = clean(substr($i, 14))
        }
      }
      if (namespace != "") {
        print kind "\t" name "\t" namespace
      }
    }
  ' "$1"
}

render_objects() {
  awk '
    BEGIN { RS = "---"; FS = "\n" }
    {
      kind = ""
      name = ""
      in_metadata = 0
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^kind: /) {
          kind = substr($i, 7)
        } else if ($i == "metadata:") {
          in_metadata = 1
        } else if (in_metadata && $i ~ /^[^ ]/) {
          in_metadata = 0
        } else if (in_metadata && name == "" && $i ~ /^  name: /) {
          name = substr($i, 9)
          gsub(/^['\''"]|['\''"]$/, "", name)
        }
      }
      if (kind != "" && name != "") {
        print kind "\t" name
      }
    }
  ' "$1"
}

assert_namespaces() {
  local manifest="$1"
  shift
  local allowed
  local kind
  local name
  local namespace

  while IFS=$'\t' read -r kind name namespace; do
    for allowed in "$@"; do
      [[ "$namespace" == "$allowed" ]] && continue 2
    done
    fail "$kind $name rendered in unexpected namespace $namespace"
  done < <(render_namespaces "$manifest")
}

default_manifest="$TEST_ROOT/default-custom-namespace.yaml"
helm template namespace-check "$TEST_CHART_DIR" \
  --namespace custom-system \
  --include-crds >"$default_manifest"
assert_namespaces "$default_manifest" custom-system kube-system

for required in \
  'name: multikueueclusters.kueue.x-k8s.io' \
  'name: multikueueconfigs.kueue.x-k8s.io' \
  'manager-clusterprofiles-role' \
  'manager-secrets-role' \
  '      - multikueueclusters' \
  '      - multikueueconfigs' \
  '--feature-gates=MultiKueue=true'; do
  grep -Fq -- "$required" "$default_manifest" ||
    fail "default render is missing the MultiKueue capability: $required"
done
retained_crd_count="$(grep -c 'helm.sh/resource-policy: keep' "$default_manifest")"
[[ "$retained_crd_count" -eq 11 ]] ||
  fail "expected all 11 Kueue CRDs to retain the Helm keep policy, got $retained_crd_count"
if grep -Eq $'^(AdmissionCheck|MultiKueueConfig|MultiKueueCluster)\t' < <(render_objects "$default_manifest"); then
  fail "default render created operator-owned MultiKueue routing objects implicitly"
fi
if grep -Ei $'^Secret\t.*(multikueue|worker)' < <(render_objects "$default_manifest"); then
  fail "default render created operator-owned worker credentials implicitly"
fi

for deployment in \
  namespace-check-kueue-controller-manager \
  kuberay-operator \
  tau-core-controller \
  tau-portal; do
  grep -Fq $'Deployment\t'"$deployment"$'\tcustom-system' < <(render_namespaces "$default_manifest") ||
    fail "$deployment did not render in custom-system"
done
grep -Eq $'^DaemonSet\tgpu-monitoring-.*\tcustom-system$' < <(render_namespaces "$default_manifest") ||
  fail "gpu-monitoring did not render in custom-system"

namespace_error="$TEST_ROOT/gpu-monitoring-namespace-error"
if helm template namespace-check "$TEST_CHART_DIR" \
  --namespace custom-system \
  --set gpu-monitoring.namespace=other-system >"$TEST_ROOT/invalid-namespace.yaml" 2>"$namespace_error"; then
  fail "gpu-monitoring rendered outside the TauGrid release namespace"
fi
grep -Fq 'gpu-monitoring.namespace is no longer supported; use --namespace to move every TauGrid system component together' "$namespace_error" ||
  fail "gpu-monitoring namespace refusal did not explain the single-namespace contract"

optional_manifest="$TEST_ROOT/optional-custom-namespace.yaml"
helm template namespace-check "$TEST_CHART_DIR" \
  --namespace custom-system \
  --set taugrid-core.prewarm.enabled=true \
  --set taugrid-core.stellar.enabled=true \
  --set taugrid-core.stellar.kusto.queryCommand=/bin/true \
  --set taugrid-core.lifecycleRecorder.enabled=true \
  --set taugrid-core.lifecycleRecorder.targetNamespace=workload-system \
  --set taugrid-core.lifecycleRecorder.cluster=test-cluster \
  --set taugrid-core.lifecycleRecorder.kusto.endpoint=https://example.kusto.windows.net \
  --set taugrid-core.lifecycleRecorder.workloadIdentity.enabled=true \
  --set taugrid-core.lifecycleRecorder.serviceAccount.create=true \
  --set taugrid-core.lifecycleRecorder.serviceAccount.name=tau-lifecycle-recorder \
  --set-string 'taugrid-core.lifecycleRecorder.serviceAccount.annotations.azure\.workload\.identity/client-id=test-client-id' \
  --set taugrid-core.lifecycleRecorder.rbac.create=true >"$optional_manifest"
assert_namespaces "$optional_manifest" custom-system kube-system workload-system

for workload in baked-image-prewarm tau-stellar tau-lifecycle-recorder; do
  grep -Eq $'^(DaemonSet|Deployment)\t'"$workload"$'\tcustom-system$' < <(render_namespaces "$optional_manifest") ||
    fail "$workload did not render in custom-system"
done

echo "TauGrid chart script tests passed"
