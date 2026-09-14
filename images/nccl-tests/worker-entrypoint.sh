#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if [[ $# -gt 0 ]]; then
  exec /opt/nvidia/nvidia_entrypoint.sh "$@"
fi

ulimit -l unlimited
ssh-keygen -A
exec /opt/nvidia/nvidia_entrypoint.sh /usr/sbin/sshd -D -e -p 2222
