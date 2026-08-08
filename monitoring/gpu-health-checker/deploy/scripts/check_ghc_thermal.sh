#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# NPD wrapper: thermal & power violation detection via gpu-health-checker.
exec /usr/local/bin/gpu-health-checker read thermal
