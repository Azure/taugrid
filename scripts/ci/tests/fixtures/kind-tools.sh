#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -eu

tool="${0##*/}"
printf '%s %s\n' "$tool" "$*" >>"$CALL_LOG"
if [ "${KIND_TEST_FAILURE:-}" = "$tool:$1" ] || [ "${KIND_TEST_FAILURE:-}" = "$1" ]; then
  # Let the importer succeed so a failed save specifically exercises pipefail.
  [ "$tool:$1" != podman:save ] || printf archive
  exit 1
fi

case "$tool:$1" in
  podman:info | docker:info) ;;
  podman:ps)
    [ "${KIND_TEST_FAILURE:-}" != empty-nodes ] || exit 0
    printf '%s\n' tau-test-control-plane tau-test-worker ;;
  docker:ps) printf '%s\n' tau-test-control-plane ;;
  podman:image) [ "$2" = inspect ]; printf '%s\n' test-image-id ;;
  podman:save) printf archive ;;
  podman:exec)
    if [ "$2" = tau-test-control-plane ] && [ "$3" = crictl ]; then
      printf '%s\n' sha256:test-image-id
    elif [ "$2" = -i ] && [ "$3" = tau-test-worker ] && [ "$4" = ctr ]; then
      [ "$(cat)" = archive ] || exit 1
      [ "${KIND_TEST_FAILURE:-}" != import ]
    else
      exit 1
    fi ;;
  kind:create | kind:delete | kind:load) ;;
  helm:template)
    printf '%s\n' 'apiVersion: apps/v1' 'kind: Deployment' 'spec:' \
      '  template:' '    spec:' '      containers:' \
      '        - image: registry.example.com/test:1' ;;
  kubectl:--context) cat >/dev/null; printf '%s' registry.example.com/test:1 ;;
  *) echo "unexpected mock command: $tool $*" >&2; exit 64 ;;
esac
