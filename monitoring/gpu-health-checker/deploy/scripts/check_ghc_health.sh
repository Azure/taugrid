#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# NPD wrapper: DCGM health diagnostics via gpu-health-checker.
# Replaces: check-dcgm-health.sh
exec /usr/local/bin/gpu-health-checker read health
