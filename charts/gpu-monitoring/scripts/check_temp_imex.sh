#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# This plugin checks if imex pod is running on the current node
readonly OK=0
readonly NONOK=1

readonly IMEX_PROCNAME="${IMEX_PROCNAME:-imex}"

# Detect GPU type — prefer env var, fallback to nvidia-smi
GPU_TYPE="${GPU_TYPE:-$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)}"
case "$GPU_TYPE" in
  *GB200*|*GB300*) ;;
  *)
    echo "Not a Blackwell node, skipping IMEX check"
    exit $OK
    ;;
esac

pgrep $IMEX_PROCNAME

if [ $? -ne 0 ]; then
  echo "Process $IMEX_PROCNAME not found."
  exit $NONOK
fi

echo "Process $IMEX_PROCNAME is running."
exit $OK
