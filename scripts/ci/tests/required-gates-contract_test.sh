#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly SCOPE_SCRIPT="${REPO_ROOT}/scripts/ci/validation-scope.sh"
readonly TEST_ROOT="${REPO_ROOT}/.ci-required-gates-test-$$"
trap 'rm -rf -- "${TEST_ROOT}"' EXIT
mkdir -p "${TEST_ROOT}"

fail() {
  echo "required gate contract test failed: $*" >&2
  exit 1
}

job_block() {
  awk -v job="$2" '
    $0 == "  " job ":" { active = 1 }
    active && /^  [[:alnum:]_-]+:$/ && $0 != "  " job ":" { exit }
    active { print }
  ' "$1"
}

run_gate() {
  env "${gate_env[@]}" "$@" bash -e -o pipefail -c "$gate_script" >/dev/null 2>&1
}

for suite in core sdk image docs kubernetes; do
  case "$suite" in
    core) workflow="${REPO_ROOT}/.github/workflows/taugrid-validation.yml"; title=Core ;;
    image) workflow="${REPO_ROOT}/.github/workflows/taugrid-image-validation.yml"; title=Images ;;
    sdk) workflow="${REPO_ROOT}/.github/workflows/taugrid-sdk-validation.yml"; title=SDK ;;
    docs) workflow="${REPO_ROOT}/.github/workflows/taugrid-docs-validation.yml"; title=Docs ;;
    kubernetes) workflow="${REPO_ROOT}/.github/workflows/taugrid-kubernetes-validation.yml"; title=Kubernetes ;;
  esac
  gate="$(job_block "$workflow" required)"
  grep -Fq "name: TauGrid $title Required" <<<"$gate" || fail "$suite check name"
  grep -Fq 'if: always()' <<<"$gate" || fail "$suite must aggregate failed dependencies"
  if grep -Eq '^    (paths|paths-ignore):|pull_request_target|continue-on-error: true' <<<"$gate"; then
    fail "$suite gate must not filter or ignore errors"
  fi
  if grep -Eq '^    (paths|paths-ignore):|pull_request_target' "$workflow"; then
    fail "$suite workflow must start for every pull request"
  fi
  grep -q '^  pull_request:$' "$workflow" || fail "$suite pull request trigger"
  grep -q '^  push:$' "$workflow" || fail "$suite push trigger"
  grep -q '^  workflow_dispatch:$' "$workflow" || fail "$suite manual trigger"
  jobs="$(awk '/^jobs:$/ { active = 1; next } active && /^  [[:alnum:]_-]+:$/ { gsub(/[ :]/, ""); if ($0 != "required") print }' "$workflow" | sort)"
  dependencies="$(awk '/^    needs:$/ { active = 1; next } active && /^      - / { print $2; next } active { exit }' <<<"$gate" | sort)"
  [[ "$jobs" == "$dependencies" ]] || fail "$suite gate omits jobs: $jobs != $dependencies"
  for job in $jobs; do
    grep -Fq "needs.$job.result" <<<"$gate" || fail "$suite does not check $job"
  done
  gate_script="$(awk '/^        run: \|$/ { active = 1; next } active { sub(/^          /, ""); print }' <<<"$gate")"
  [[ -n "$gate_script" ]] || fail "$suite gate script not found"
  result_vars="$(awk '/^          [A-Z_]+_RESULT:/ { sub(/:$/, "", $1); print $1 }' <<<"$gate")"
  gate_env=(GITHUB_EVENT_NAME=pull_request RELEASE_PREFLIGHT_ENABLED=true SCOPE_ENABLED=true)
  for variable in $result_vars; do
    gate_env+=("$variable=success")
  done
  run_gate || fail "$suite rejects successful validation"
  for variable in $result_vars; do
    for result in failure cancelled skipped; do
      if run_gate "$variable=$result"; then
        fail "$suite accepts $variable=$result when selected"
      fi
    done
  done
  case "$suite" in
    core)
      release_skips=(
        RELEASE_PREFLIGHT_ENABLED=false
        RELEASE_BUILD_RESULT=skipped
        RELEASE_INSTALLER_RESULT=skipped
        RELEASE_WINDOWS_INSTALLER_RESULT=skipped
      )
      run_gate "${release_skips[@]}" || fail "core rejects unselected release preflight"
      for variable in RELEASE_BUILD_RESULT RELEASE_INSTALLER_RESULT RELEASE_WINDOWS_INSTALLER_RESULT; do
        if run_gate "${release_skips[@]}" "$variable=failure"; then
          fail "core ignores unexpected unselected release failure"
        fi
      done
      for event in push schedule workflow_dispatch; do
        run_gate GITHUB_EVENT_NAME="$event" DEPENDENCY_REVIEW_RESULT=skipped ||
          fail "core rejects intentional dependency review skip on $event"
        if run_gate GITHUB_EVENT_NAME="$event" DEPENDENCY_REVIEW_RESULT=failure; then
          fail "core ignores dependency review failure on $event"
        fi
      done
      grep -Fq 'bash scripts/ci/tests/required-gates-contract_test.sh' \
        <<<"$(job_block "$workflow" license-headers)" || fail "core must run gate contracts"
      ;;
    sdk) ;;
    *)
      scope="$(job_block "$workflow" scope)"
      grep -Fq "run: bash scripts/ci/validation-scope.sh $suite" <<<"$scope" ||
        fail "$suite scope helper"
      grep -Fq 'fetch-depth: 0' <<<"$scope" || fail "$suite needs complete diff history"
      grep -Fq "BASE_SHA: \${{ github.event.pull_request.base.sha || github.event.before }}" <<<"$scope" ||
        fail "$suite base SHA"
      grep -Fq "HEAD_SHA: \${{ github.event.pull_request.head.sha || github.sha }}" <<<"$scope" ||
        fail "$suite head SHA"
      skips=(SCOPE_ENABLED=false)
      for variable in $result_vars; do
        [[ "$variable" == SCOPE_RESULT ]] || skips+=("$variable=skipped")
      done
      run_gate "${skips[@]}" || fail "$suite rejects intentionally skipped suite"
      for variable in $result_vars; do
        for result in failure cancelled; do
          if run_gate "${skips[@]}" "$variable=$result"; then
            fail "$suite ignores unselected $variable=$result"
          fi
        done
        if [[ "$variable" != SCOPE_RESULT ]] && run_gate "${skips[@]}" "$variable=success"; then
          fail "$suite accepts unexpected unselected $variable=success"
        fi
      done
      for enabled in "" invalid; do
        if run_gate "${skips[@]}" SCOPE_ENABLED="$enabled"; then
          fail "$suite accepts invalid scope output"
        fi
      done
      for job in $jobs; do
        [[ "$job" == scope ]] && continue
        block="$(job_block "$workflow" "$job")"
        grep -Fq "if: needs.scope.outputs.enabled == 'true'" <<<"$block" ||
          fail "$suite/$job must honor scope"
        grep -Eq '^    needs: scope$|^      - scope$' <<<"$block" ||
          fail "$suite/$job must depend on scope"
      done
      ;;
  esac
done

grep -Fq 'bash scripts/ci/tests/required-gates-contract_test.sh' "${REPO_ROOT}/Makefile" ||
  fail "make test must include gate contracts"

cd "${TEST_ROOT}"
git init -q -b main
git config user.email ci-contract@example.invalid
git config user.name CI-contract
git config commit.gpgsign false
git config core.hooksPath /dev/null
git config core.autocrlf false

commit_files() {
  git add -A
  git commit -qm fixture
}

printf 'initial\n' > unrelated.txt
commit_files
initial="$(git rev-parse HEAD)"

check_scope() {
  local suite="$1" expected="$2" event="$3" base="$4" head="$5"
  : > scope-output
  GITHUB_EVENT_NAME="$event" BASE_SHA="$base" HEAD_SHA="$head" GITHUB_OUTPUT="$PWD/scope-output" \
    bash "$SCOPE_SCRIPT" "$suite"
  [[ "$(cat scope-output)" == "enabled=$expected" ]] ||
    fail "$suite expected $expected for $event $base..$head"
  rm scope-output
}

while IFS='|' read -r path selected; do
  before="$(git rev-parse HEAD)"
  mkdir -p "$(dirname "$path")"
  printf 'changed\n' > "$path"
  commit_files
  after="$(git rev-parse HEAD)"
  for suite in image docs kubernetes; do
    expected=false
    case ",$selected," in
      *,"$suite",*) expected=true ;;
    esac
    check_scope "$suite" "$expected" pull_request "$before" "$after"
    check_scope "$suite" "$expected" push "$before" "$after"
  done
done <<'CASES'
unrelated/change.txt|
README.md|docs
docs/nested/page.md|docs
site/config.toml|docs
site/go.mod|image,docs
cli/main.go|image,kubernetes
core/main.go|image,kubernetes
controllers/tau-core/main.go|image,kubernetes
portal/main.go|image
monitoring/gpu-health-checker/main.go|image
monitoring/gpu-metrics-collector/main.go|image
metrics/experiment-metrics-collector/main.go|image
images/tau/Dockerfile|image
images/tau-core-controller/Dockerfile|image,kubernetes
images/taugrid-portal/Dockerfile|image
images/taugrid-metrics-collector/Dockerfile|image
images/gpu-metrics-collector/Dockerfile|image
images/adx-mon/Dockerfile|image
.go-version|image
.dockerignore|image
tests/e2e/go.mod|image
scripts/ci/tests/image-build-contract_test.sh|image
scripts/ci/tests/go-runtime-default-contract_test.sh|image
scripts/ci/validate-image-build-contract.sh|image
scripts/lib/image-specs.sh|image
charts/tau-core-controller/values.yaml|kubernetes
charts/taugrid/values.yaml|kubernetes
charts/taugrid-core/values.yaml|kubernetes
examples/job.yaml|kubernetes
scripts/ci/vendor-taugrid-dependencies.sh|kubernetes
.github/workflows/taugrid-image-validation.yml|image
.github/workflows/taugrid-docs-validation.yml|docs
.github/workflows/taugrid-kubernetes-validation.yml|kubernetes
scripts/ci/validation-scope.sh|image,docs,kubernetes
scripts/ci/tests/required-gates-contract_test.sh|image,docs,kubernetes
CASES

before="$(git rev-parse HEAD)"
git mv cli/main.go unrelated/moved.go
commit_files
after="$(git rev-parse HEAD)"
check_scope image true pull_request "$before" "$after"
check_scope kubernetes true pull_request "$before" "$after"
check_scope docs false pull_request "$before" "$after"

before="$after"
git rm -q README.md
commit_files
after="$(git rev-parse HEAD)"
check_scope docs true pull_request "$before" "$after"
check_scope image false pull_request "$before" "$after"
check_scope kubernetes false pull_request "$before" "$after"

before="$after"
printf 'newline path\n' > $'core/file with\nnewline.go'
commit_files
after="$(git rev-parse HEAD)"
check_scope image true pull_request "$before" "$after"
check_scope kubernetes true pull_request "$before" "$after"
check_scope docs false pull_request "$before" "$after"
for suite in image docs kubernetes; do
  check_scope "$suite" true push "$initial" "$after"
  check_scope "$suite" false push "$after" "$after"
  check_scope "$suite" true push 0000000000000000000000000000000000000000 "$after"
  check_scope "$suite" true push "" "$after"
  check_scope "$suite" true schedule "" ""
  check_scope "$suite" true workflow_dispatch "" ""
done

git checkout -q -b advanced-base "$initial"
mkdir -p core
printf 'base branch only\n' > core/base-only.go
commit_files
advanced_base="$(git rev-parse HEAD)"
git checkout -q -b fork-head "$initial"
printf 'fork change\n' > fork-unrelated.txt
commit_files
fork_head="$(git rev-parse HEAD)"
check_scope image false pull_request "$advanced_base" "$fork_head"
check_scope kubernetes false pull_request "$advanced_base" "$fork_head"
check_scope image true push "$advanced_base" "$fork_head"
if GITHUB_EVENT_NAME=pull_request BASE_SHA="$initial" HEAD_SHA=invalid GITHUB_OUTPUT="$PWD/scope-output" \
  bash "$SCOPE_SCRIPT" image >/dev/null 2>&1; then
  fail "invalid head must fail closed"
fi
if GITHUB_EVENT_NAME=unknown GITHUB_OUTPUT="$PWD/scope-output" \
  bash "$SCOPE_SCRIPT" image >/dev/null 2>&1; then
  fail "unsupported event must fail closed"
fi
if GITHUB_EVENT_NAME=workflow_dispatch GITHUB_OUTPUT="$PWD/scope-output" \
  bash "$SCOPE_SCRIPT" invalid >/dev/null 2>&1; then
  fail "unknown suite must fail closed"
fi

echo "Required gate contracts and change detection passed"
