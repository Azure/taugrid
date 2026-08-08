#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# NPD wrapper: clock throttle detection via gpu-health-checker.
# Replaces: check_gpu_throttle.sh
exec /usr/local/bin/gpu-health-checker read clocks
