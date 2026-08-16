#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Wrapper to execute nvidia-smi on the host via nsenter.
# The NPD container doesn't have nvidia-smi installed; this wrapper
# uses nsenter to run it in the host's mount namespace (requires
# hostPID: true and privileged: true).
exec nsenter --target 1 --mount -- nvidia-smi "$@"
