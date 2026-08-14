#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# DCGM runs on the host; execute its CLI in the host mount namespace.
exec nsenter --target 1 --mount -- dcgmi "$@"
