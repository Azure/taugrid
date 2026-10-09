#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly RECOVER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-nightly-recover.sh"

fail() {
  echo "TauGrid unbounded-stable nightly recovery test failed: $*" >&2
  exit 1
}

fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT
mkdir -p "${fixture}/bin" "${fixture}/diagnostics"
export PATH="${fixture}/bin:${PATH}"
export TAUGRID_RELEASE=taugrid
export TAUGRID_SYSTEM_NAMESPACE=tau-system
export TAUGRID_KUBE_CONTEXT=unbounded-stable
export TAUGRID_DIAGNOSTICS_DIR="${fixture}/diagnostics"
export FAKE_HELM_STATE="${fixture}/helm-state"
export FAKE_HELM_LOG="${fixture}/helm.log"
export FAKE_TAU_LOG="${fixture}/tau.log"

cat >"${fixture}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_HELM_LOG}"
case "$1" in
  list)
    if [[ "$(cat "${FAKE_HELM_STATE}")" == "present" ]]; then
      printf '[{"name":"taugrid","namespace":"tau-system","revision":"8","status":"deployed"}]\n'
    else
      printf '[]\n'
    fi
    ;;
  rollback)
    ;;
  uninstall)
    printf 'absent\n' >"${FAKE_HELM_STATE}"
    ;;
  *)
    exit 1
    ;;
esac
EOF

cat >"${fixture}/bin/tau" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${FAKE_TAU_LOG}"
EOF
chmod +x "${fixture}/bin/helm" "${fixture}/bin/tau"
export TAUGRID_CLI="${fixture}/bin/tau"

printf 'present\n' >"${FAKE_HELM_STATE}"
export TAUGRID_PREVIOUS_REVISION=7
result="$("${RECOVER}" recover)"
[[ "${result}" == "restored and validated revision 7" ]] ||
  fail "existing release recovery result was ${result}"
grep -Fq "list --namespace tau-system --kube-context unbounded-stable --deployed --failed --pending --uninstalled --uninstalling --superseded --output json" \
  "${FAKE_HELM_LOG}" ||
  fail "release inspection must use Helm 3 and Helm 4 compatible state flags"
if grep -Eq -- '--pending-(install|upgrade|rollback)' "${FAKE_HELM_LOG}"; then
  fail "release inspection must not use unsupported granular pending flags"
fi
grep -Fq "rollback taugrid 7" "${FAKE_HELM_LOG}" ||
  fail "failed install readiness must roll back the previous revision"
grep -Fq "cluster validate installation" "${FAKE_TAU_LOG}" ||
  fail "the restored release must be validated"

: >"${FAKE_HELM_LOG}"
: >"${FAKE_TAU_LOG}"
printf 'present\n' >"${FAKE_HELM_STATE}"
export TAUGRID_PREVIOUS_REVISION=0
result="$("${RECOVER}" recover)"
[[ "${result}" == "removed the rejected first installation" ]] ||
  fail "first installation recovery result was ${result}"
grep -Fq "uninstall taugrid" "${FAKE_HELM_LOG}" ||
  fail "a rejected first installation must be removed"
[[ "$(cat "${FAKE_HELM_STATE}")" == "absent" ]] ||
  fail "the rejected first installation remained present"
[[ ! -s "${FAKE_TAU_LOG}" ]] ||
  fail "an absent release must not run installation validation"

cat >"${fixture}/bin/helm" <<'EOF'
#!/usr/bin/env bash
echo "simulated Helm inspection failure" >&2
exit 1
EOF
chmod +x "${fixture}/bin/helm"
if "${RECOVER}" inspect >/dev/null 2>"${fixture}/inspect-error.txt"; then
  fail "Helm inspection errors must not be treated as an absent release"
fi
grep -Fq "simulated Helm inspection failure" "${fixture}/inspect-error.txt" ||
  fail "inspection failure was not surfaced"
export TAUGRID_PREVIOUS_REVISION=0
if "${RECOVER}" recover >"${fixture}/recover-output.txt" 2>"${fixture}/recover-error.txt"; then
  fail "recovery must fail when Helm inventory inspection fails"
fi
grep -Fq "cannot recover without a valid Helm release inventory" \
  "${fixture}/recover-error.txt" ||
  fail "recovery did not surface the invalid inventory"
if grep -Fq "removed the rejected first installation" "${fixture}/recover-output.txt"; then
  fail "recovery falsely reported success after an inspection failure"
fi

echo "TauGrid unbounded-stable nightly recovery tests passed"
