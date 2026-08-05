#!/bin/bash
# NPD wrapper: DCGM health diagnostics via gpu-health-checker.
# Replaces: check-dcgm-health.sh
exec /usr/local/bin/gpu-health-checker read health
