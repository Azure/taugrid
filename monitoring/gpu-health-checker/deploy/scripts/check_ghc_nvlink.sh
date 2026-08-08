#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# NPD wrapper: NVLink status check via gpu-health-checker.
# Replaces: check_gpu_nvlink_b200.sh
exec /usr/local/bin/gpu-health-checker read nvlink
