#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly KUEUE_COMMIT="8eab68778fc1b52affe165fdf5af29d1e9b4f3cb"
readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly FIXTURE_DIR="${ROOT_DIR}/scripts/ci/kueue-tests"

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

curl --fail --location --silent --show-error \
  "https://github.com/kubernetes-sigs/kueue/archive/${KUEUE_COMMIT}.tar.gz" |
  tar -xz -C "${work_dir}"

kueue_dir="${work_dir}/kueue-${KUEUE_COMMIT}"
for fixture in "${FIXTURE_DIR}"/*_test.go.fixture; do
  name="$(basename "${fixture}" .fixture)"
  cp "${fixture}" "${kueue_dir}/pkg/scheduler/${name}"
done
(
  cd "${kueue_dir}"
  go test ./pkg/scheduler -run '^TestTauGrid' -count=1 -v
)
