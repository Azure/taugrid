#!/bin/bash
# NPD wrapper: XID error detection via gpu-health-checker.
# Replaces: check_gpu_xid.sh
exec /usr/local/bin/gpu-health-checker read xid
