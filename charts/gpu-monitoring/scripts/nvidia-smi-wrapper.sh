#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

exec nsenter --target 1 --mount -- nvidia-smi "$@"
