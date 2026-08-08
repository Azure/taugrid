#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# NPD wrapper: GPU inventory & config validation via gpu-health-checker.
# Replaces: check_gpu_count.sh, check_gpu_vbios.sh, check_gpu_driver.sh, check-nvidia-smi.sh, check-nvidia-device-files.sh
exec /usr/local/bin/gpu-health-checker read info
