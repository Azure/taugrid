#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

TEST_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEST_DIR
readonly TEST_ROOT="${TEST_DIR}/.publish-helm-charts-test-$$"
readonly ISOLATED_REPO="${TEST_ROOT}/repo"
readonly PACKAGE_DIR="${ISOLATED_REPO}/packages"
readonly COMMAND_LOG="${TEST_ROOT}/commands.log"
REAL_HELM_BIN="$(command -v helm)"
readonly REAL_HELM_BIN
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
  echo "publish-helm-charts test failed: $*" >&2
  exit 1
}

assert_output_contains() {
  local output="$1"
  local expected="$2"
  [[ "$output" == *"$expected"* ]] ||
    fail "expected output to contain '$expected', got: $output"
}

assert_log_contains() {
  local expected="$1"
  grep -Fq "$expected" "$COMMAND_LOG" ||
    fail "expected command log to contain '$expected'"
}

assert_log_excludes() {
  local unexpected="$1"
  if grep -Fq "$unexpected" "$COMMAND_LOG"; then
    fail "expected command log not to contain '$unexpected'"
  fi
}

assert_log_count() {
  local expected_count="$1"
  local pattern="$2"
  local actual_count
  actual_count=$(grep -Fc "$pattern" "$COMMAND_LOG" || true)
  [[ "$actual_count" -eq "$expected_count" ]] ||
    fail "expected $expected_count occurrences of '$pattern', got $actual_count"
}

mkdir -p \
  "${ISOLATED_REPO}/scripts/ci" \
  "${ISOLATED_REPO}/charts/tau-core-controller" \
  "${ISOLATED_REPO}/charts/gpu-monitoring" \
  "${ISOLATED_REPO}/charts/taugrid-core" \
  "${ISOLATED_REPO}/charts/adx-mon" \
  "${ISOLATED_REPO}/charts/taugrid" \
  "${ISOLATED_REPO}/image-fixture/templates" \
  "$PACKAGE_DIR" \
  "${TEST_ROOT}/bin"
cp "${TEST_DIR}/../publish-helm-charts.sh" "${ISOLATED_REPO}/scripts/ci/"

write_chart() {
  local name="$1"
  local version="$2"

  cat >"${ISOLATED_REPO}/charts/${name}/Chart.yaml" <<EOF
apiVersion: v2
name: ${name}
version: ${version}
EOF
}

write_chart tau-core-controller 0.3.0
write_chart gpu-monitoring 0.1.4
write_chart taugrid-core 0.3.0
write_chart adx-mon 0.3.3
write_chart taugrid 0.3.0

cp "${ISOLATED_REPO}/charts/taugrid/Chart.yaml" "${ISOLATED_REPO}/image-fixture/"
cat >"${ISOLATED_REPO}/image-fixture/templates/workloads.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: portal
spec:
  template:
    spec:
      initContainers:
        - name: init
          image: mcr.microsoft.com/aks/ai-runtime/tau:0.3.0
      containers:
        - name: portal
          image: {{ .Values.portalImage | default "mcr.microsoft.com/aks/ai-runtime/taugrid-portal:0.3.0" | quote }}
        - name: metrics
          image: mcr.microsoft.com/aks/ai-runtime/metrics:0.3.0
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: nested
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: portal
              image: mcr.microsoft.com/aks/ai-runtime/taugrid-portal:0.3.0
---
apiVersion: v1
kind: Pod
metadata:
  name: hook
  annotations:
    helm.sh/hook: pre-install
spec:
  containers:
    - name: hook
      image: mcr.microsoft.com/aks/ai-runtime/hook@sha256:0123456789abcdef
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: fixtures.example.invalid
spec:
  versions:
    - name: v1
      schema:
        openAPIV3Schema:
          properties:
            containers:
              type: array
              required: [name, image]
              items:
                properties:
                  image:
                    type: string
              default:
                - image: unpublished.invalid/crd-default:0.3.0
{{ if .Values.disabledWorkload }}
---
apiVersion: v1
kind: Pod
metadata:
  name: disabled
spec:
  containers:
    - name: disabled
      image: unpublished.invalid/disabled:0.3.0
{{ end }}
EOF
"$REAL_HELM_BIN" package "${ISOLATED_REPO}/image-fixture" --destination "$PACKAGE_DIR" >/dev/null

cat >"${TEST_ROOT}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'helm %s\n' "$*" >>"$COMMAND_LOG"
case "$1 $2" in
  "show chart")
    cat "$3/Chart.yaml"
    ;;
  "push "*)
    ;;
  "pull "*)
    ;;
  "template "*)
    if [[ "${FAKE_RENDER_FAILURE:-}" == true ]]; then
      echo "chart rendering failed" >&2
      exit 1
    fi
    "$REAL_HELM_BIN" "$@"
    ;;
  *)
    echo "unexpected helm command: $*" >&2
    exit 64
    ;;
esac
EOF
chmod +x "${TEST_ROOT}/bin/helm"

cat >"${TEST_ROOT}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'docker %s\n' "$*" >>"$COMMAND_LOG"
[[ "$1 $2 $3" == "buildx imagetools inspect" ]]
if [[ "${FAKE_MISSING_IMAGE:-}" == "$4" ]]; then
  echo "manifest unknown" >&2
  exit 1
fi
EOF
chmod +x "${TEST_ROOT}/bin/docker"

cat >"${TEST_ROOT}/bin/az" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf 'az %s\n' "$*" >>"$COMMAND_LOG"

if [[ "$1 $2" == "acr login" ]]; then
  exit 0
fi

if [[ "$1 $2 $3" == "acr repository show-tags" ]]; then
  repository=""
  while [[ "$#" -gt 0 ]]; do
    if [[ "$1" == "--repository" ]]; then
      repository="$2"
      break
    fi
    shift
  done
  chart_name="${repository##*/}"

  if [[ "${FAKE_ERROR_CHART:-}" == "$chart_name" ]]; then
    echo "authorization failed" >&2
    exit 1
  fi

  version=$(awk '$1 == "version:" {gsub(/^"|"$/, "", $2); print $2; exit}' \
    "charts/${chart_name}/Chart.yaml")
  if [[ "${FAKE_UNPUBLISHED_CHART:-}" == "$chart_name" ||
    ",${FAKE_UNPUBLISHED_CHARTS:-}," == *",${chart_name},"* ]]; then
    echo "0.0.1"
  else
    echo "$version"
  fi
  exit 0
fi

if [[ "$1 $2 $3" == "acr repository show" ]]; then
  echo "sha256:0123456789abcdef"
  exit 0
fi

echo "unexpected az command: $*" >&2
exit 64
EOF
chmod +x "${TEST_ROOT}/bin/az"

export COMMAND_LOG REAL_HELM_BIN
export ACR_NAME=testacr
PATH="${TEST_ROOT}/bin:${PATH}"
export PATH

run_publish() {
  (
    cd "$ISOLATED_REPO"
    scripts/ci/publish-helm-charts.sh publish "$PACKAGE_DIR"
  )
}

: >"$COMMAND_LOG"
existing_output=$(run_publish)
for chart in tau-core-controller gpu-monitoring taugrid-core adx-mon taugrid; do
  version=$(awk '$1 == "version:" {print $2; exit}' \
    "${ISOLATED_REPO}/charts/${chart}/Chart.yaml")
  assert_output_contains "$existing_output" \
    "Chart ${chart}:${version} already exists in the publish registry; skipping."
done
assert_log_excludes "az acr login"
assert_log_excludes "helm push"
assert_log_excludes "helm pull"
assert_log_excludes "docker"

: >"$COMMAND_LOG"
export FAKE_UNPUBLISHED_CHART=taugrid
new_version_output=$(run_publish)
unset FAKE_UNPUBLISHED_CHART
assert_output_contains "$new_version_output" \
  "Published and verified taugrid:0.3.0 at sha256:0123456789abcdef"
assert_log_count 1 "az acr login"
assert_log_count 1 "helm push"
assert_log_contains \
  "helm push ${PACKAGE_DIR}/taugrid-0.3.0.tgz oci://testacr.azurecr.io/public/aks/ai-runtime/helm"
assert_log_contains \
  "az acr repository show --name testacr --image public/aks/ai-runtime/helm/taugrid:0.3.0"
assert_log_contains \
  "helm pull oci://testacr.azurecr.io/public/aks/ai-runtime/helm/taugrid --version 0.3.0"
assert_log_contains "helm template image-preflight ${PACKAGE_DIR}/taugrid-0.3.0.tgz --include-crds"
assert_log_count 4 "docker buildx imagetools inspect"
assert_log_count 1 "docker buildx imagetools inspect mcr.microsoft.com/aks/ai-runtime/taugrid-portal:0.3.0"
assert_log_contains "docker buildx imagetools inspect mcr.microsoft.com/aks/ai-runtime/tau:0.3.0"
assert_log_contains "docker buildx imagetools inspect mcr.microsoft.com/aks/ai-runtime/metrics:0.3.0"
assert_log_contains "docker buildx imagetools inspect mcr.microsoft.com/aks/ai-runtime/hook@sha256:0123456789abcdef"
assert_log_excludes "unpublished.invalid/disabled"
assert_log_excludes "unpublished.invalid/crd-default"

export FAKE_UNPUBLISHED_CHART=taugrid
for image in taugrid-portal tau metrics; do
  : >"$COMMAND_LOG"
  export FAKE_MISSING_IMAGE="mcr.microsoft.com/aks/ai-runtime/${image}:0.3.0"
  if missing_image_output=$(run_publish 2>&1); then
    fail "publishing with a missing $image image unexpectedly succeeded"
  fi
  assert_output_contains "$missing_image_output" "Required image is not available: $FAKE_MISSING_IMAGE"
  assert_log_excludes "az acr login"
  assert_log_excludes "helm push"
done
unset FAKE_MISSING_IMAGE

: >"$COMMAND_LOG"
export FAKE_UNPUBLISHED_CHARTS=tau-core-controller,taugrid
"$REAL_HELM_BIN" package "${ISOLATED_REPO}/charts/tau-core-controller" --destination "$PACKAGE_DIR" >/dev/null
export FAKE_MISSING_IMAGE=mcr.microsoft.com/aks/ai-runtime/taugrid-portal:0.3.0
if multiple_chart_output=$(run_publish 2>&1); then
  fail "publishing multiple charts with a missing image unexpectedly succeeded"
fi
unset FAKE_UNPUBLISHED_CHARTS FAKE_MISSING_IMAGE
assert_output_contains "$multiple_chart_output" "Required image is not available"
assert_log_contains "helm template image-preflight ${PACKAGE_DIR}/tau-core-controller-0.3.0.tgz --include-crds"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

: >"$COMMAND_LOG"
export FAKE_RENDER_FAILURE=true
if render_failure_output=$(run_publish 2>&1); then
  fail "publishing with a broken render unexpectedly succeeded"
fi
unset FAKE_RENDER_FAILURE FAKE_UNPUBLISHED_CHART
assert_output_contains "$render_failure_output" "chart rendering failed"
assert_log_excludes "docker"
assert_log_excludes "helm push"

: >"$COMMAND_LOG"
export FAKE_MISSING_IMAGE=mcr.microsoft.com/aks/ai-runtime/hook@sha256:0123456789abcdef
if hook_image_output=$(
  "${ISOLATED_REPO}/scripts/ci/publish-helm-charts.sh" verify-images \
    "${PACKAGE_DIR}/taugrid-0.3.0.tgz" 2>&1
); then
  fail "image verification with a missing hook image unexpectedly succeeded"
fi
unset FAKE_MISSING_IMAGE
assert_output_contains "$hook_image_output" "Required image is not available"
assert_log_excludes "az acr"
assert_log_excludes "helm push"

cat >"${ISOLATED_REPO}/image-fixture/templates/invalid.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: invalid
spec:
  containers:
    - name: missing-image
      image: ""
EOF
"$REAL_HELM_BIN" package "${ISOLATED_REPO}/image-fixture" --destination "$PACKAGE_DIR" >/dev/null
: >"$COMMAND_LOG"
export FAKE_UNPUBLISHED_CHART=taugrid
if invalid_image_output=$(run_publish 2>&1); then
  fail "publishing with an empty image reference unexpectedly succeeded"
fi
unset FAKE_UNPUBLISHED_CHART
assert_output_contains "$invalid_image_output" "Invalid required image"
assert_log_excludes "helm push"

: >"$COMMAND_LOG"
export FAKE_ERROR_CHART=tau-core-controller
if lookup_error_output=$(run_publish 2>&1); then
  fail "unexpected registry lookup success"
fi
unset FAKE_ERROR_CHART
assert_output_contains "$lookup_error_output" "authorization failed"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

: >"$COMMAND_LOG"
rm -f "${PACKAGE_DIR}/taugrid-0.3.0.tgz"
export FAKE_UNPUBLISHED_CHART=taugrid
if missing_archive_output=$(run_publish 2>&1); then
  fail "publishing without the candidate archive unexpectedly succeeded"
fi
unset FAKE_UNPUBLISHED_CHART
assert_output_contains "$missing_archive_output" \
  "Expected chart package not found: ${PACKAGE_DIR}/taugrid-0.3.0.tgz"
assert_log_excludes "az acr login"
assert_log_excludes "helm push"

echo "publish-helm-charts tests passed"
