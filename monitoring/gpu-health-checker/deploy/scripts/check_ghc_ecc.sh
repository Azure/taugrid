#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# NPD wrapper: ECC error detection via gpu-health-checker.
# Replaces: check_gpu_ecc.sh, check_gpu_ecc_remap_pending.sh, check_gpu_ecc_remap_failure.sh
exec /usr/local/bin/gpu-health-checker read ecc
