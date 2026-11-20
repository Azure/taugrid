#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly IMAGE_BUILDER="${REPO_ROOT}/scripts/ci/taugrid-unbounded-stable-build-image.sh"

fail() {
  echo "TauGrid unbounded-stable image builder test failed: $*" >&2
  exit 1
}

fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT
mkdir -p "${fixture}/bin"

cat >"${fixture}/bin/az" <<'EOF'
#!/usr/bin/env bash
printf 'az %s\n' "$*"
EOF
cat >"${fixture}/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*"
EOF
cat >"${fixture}/bin/make" <<'EOF'
#!/usr/bin/env bash
printf 'make %s\n' "$*"
EOF
cat >"${fixture}/bin/git" <<'EOF'
#!/usr/bin/env bash
printf '2026-11-20T00:00:00Z\n'
EOF
chmod +x \
  "${fixture}/bin/az" \
  "${fixture}/bin/docker" \
  "${fixture}/bin/git" \
  "${fixture}/bin/make"

run_component() {
  local component="$1"
  PATH="${fixture}/bin:${PATH}" \
    UNBOUNDED_STABLE_ACR_NAME=registry \
    NIGHTLY_IMAGE_REPOSITORY_PREFIX=nightly \
    BUILD_SOURCEVERSION=0123456789abcdef \
    BUILD_BUILDID=1234 \
    SYSTEM_JOBATTEMPT=2 \
    "$IMAGE_BUILDER" "$component"
}

controller_output="$(run_component controller)"
grep -Fq "docker buildx create --name taugrid-nightly-controller-1234-2" <<<"$controller_output" ||
  fail "controller build must use an isolated builder"
grep -Fq "make -C images/tau-core-controller docker-push" <<<"$controller_output" ||
  fail "controller build must use the controller Makefile"
grep -Fq "IMG=registry.azurecr.io/nightly/tau-core-controller:nightly-0123456789ab-1234" <<<"$controller_output" ||
  fail "controller build must use the immutable source-and-build tag"

portal_output="$(run_component portal)"
grep -Fq "make -C images/taugrid-portal docker-push" <<<"$portal_output" ||
  fail "portal build must use the portal Makefile"
grep -Fq "IMG=registry.azurecr.io/nightly/taugrid-portal:nightly-0123456789ab-1234" <<<"$portal_output" ||
  fail "portal build must use the immutable source-and-build tag"
grep -Fq "COMMIT=0123456789abcdef" <<<"$portal_output" ||
  fail "portal build must retain source provenance"

collector_output="$(run_component collector)"
grep -Fq "make -C images/gpu-metrics-collector docker-push" <<<"$collector_output" ||
  fail "collector build must use the GPU collector Makefile"
grep -Fq "PUSH_IMG=registry.azurecr.io/nightly/gpu-metrics-collector:nightly-0123456789ab-1234" <<<"$collector_output" ||
  fail "collector build must use the immutable source-and-build tag"

if run_component unsupported >/dev/null 2>&1; then
  fail "unsupported components must fail"
fi

echo "TauGrid unbounded-stable image builder tests passed"
