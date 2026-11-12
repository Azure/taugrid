#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

nightly_routing_value() {
  if [[ -z "$1" || "$1" == '$('*')' ]]; then
    printf '%s' "$2"
  else
    printf '%s' "$1"
  fi
}

export FLEX_NIGHTLY_WORKSPACE
export FLEX_NIGHTLY_CLUSTER_QUEUE
export FLEX_NIGHTLY_A100_FLAVOR
export FLEX_NIGHTLY_H200_FLAVOR
export FLEX_NIGHTLY_DGX_FLAVOR
export FLEX_NIGHTLY_A100_CLUSTER_QUEUE
export FLEX_NIGHTLY_H200_CLUSTER_QUEUE
export FLEX_NIGHTLY_DGX_CLUSTER_QUEUE

FLEX_NIGHTLY_WORKSPACE="$(nightly_routing_value "${FLEX_NIGHTLY_WORKSPACE:-}" nightly)"
FLEX_NIGHTLY_CLUSTER_QUEUE="$(nightly_routing_value "${FLEX_NIGHTLY_CLUSTER_QUEUE:-}" tau-gpu-cq)"
FLEX_NIGHTLY_A100_FLAVOR="$(nightly_routing_value "${FLEX_NIGHTLY_A100_FLAVOR:-}" tau-gpu-a100-80gb-v2)"
FLEX_NIGHTLY_H200_FLAVOR="$(nightly_routing_value "${FLEX_NIGHTLY_H200_FLAVOR:-}" tau-gpu-h200-141gb-v2)"
FLEX_NIGHTLY_DGX_FLAVOR="$(nightly_routing_value "${FLEX_NIGHTLY_DGX_FLAVOR:-}" tau-gpu-dgx-spark-v2)"
FLEX_NIGHTLY_A100_CLUSTER_QUEUE="$(nightly_routing_value "${FLEX_NIGHTLY_A100_CLUSTER_QUEUE:-}" tau-gpu-cq)"
FLEX_NIGHTLY_H200_CLUSTER_QUEUE="$(nightly_routing_value "${FLEX_NIGHTLY_H200_CLUSTER_QUEUE:-}" tau-gpu-cq)"
FLEX_NIGHTLY_DGX_CLUSTER_QUEUE="$(nightly_routing_value "${FLEX_NIGHTLY_DGX_CLUSTER_QUEUE:-}" tau-gpu-cq)"
