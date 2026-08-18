#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly TESTS_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

exec python3 -B -m unittest discover \
  --start-directory "${TESTS_DIR}/script_tests" \
  --pattern 'test_*.py' \
  --verbose
