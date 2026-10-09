#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# Run govulncheck on the toolchain CI pins, not on whatever Go the host has.
#
# A newer local toolchain carries a different standard library, so it reports a
# different advisory set than CI: `GOTOOLCHAIN=local` on go1.27.0 flagged nine
# stdlib findings that the pinned go1.26.9 build does not have. That made a
# correct fix look broken locally. Reproduce CI instead of guessing.
#
# Requires network access the first time: GOTOOLCHAIN downloads the pinned
# toolchain.
#
# Usage:
#   scripts/ci/govulncheck-pinned.sh --print     # show the toolchain it will use
#   scripts/ci/govulncheck-pinned.sh cli         # scan one module
#   scripts/ci/govulncheck-pinned.sh .           # scan the current directory

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
version="$(tr -d '[:space:]' <"${repo_root}/.go-version")"

if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Refusing to run: .go-version does not hold a semantic version (got '${version}')." >&2
  exit 1
fi

if [[ "${1:-}" == "--print" ]]; then
  printf 'GOTOOLCHAIN=go%s\n' "${version}"
  exit 0
fi

module_dir="${1:-.}"
cd "${module_dir}"
echo "govulncheck under GOTOOLCHAIN=go${version} (the .go-version CI pin), not the host toolchain" >&2
GOTOOLCHAIN="go${version}" go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
