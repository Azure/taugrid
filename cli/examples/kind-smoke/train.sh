#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

echo "tau kind smoke start"
echo "namespace=${TAU_NAMESPACE:-unknown}"
echo "output_dir=${TAU_OUTPUT_DIR:-ephemeral}"
echo "tau kind smoke complete"
