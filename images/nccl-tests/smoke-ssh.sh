#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly IMAGE="${1:?usage: smoke-ssh.sh <image>}"
readonly SUFFIX="$$"
readonly NETWORK="nccl-tests-ssh-smoke-${SUFFIX}"
readonly KEY_VOLUME="nccl-tests-ssh-keys-${SUFFIX}"
readonly WORKER="nccl-tests-ssh-worker-${SUFFIX}"

# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
  docker rm -f "$WORKER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  docker volume rm "$KEY_VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$NETWORK" >/dev/null
docker volume create "$KEY_VOLUME" >/dev/null
docker run --rm \
  --volume "${KEY_VOLUME}:/keys" \
  --entrypoint bash \
  "$IMAGE" -lc '
    set -euo pipefail
    ssh-keygen -q -t ed25519 -N "" -f /keys/id_ed25519
    cp /keys/id_ed25519.pub /keys/authorized_keys
    chmod 0700 /keys
    chmod 0600 /keys/id_ed25519 /keys/authorized_keys
  '

docker run --detach \
  --name "$WORKER" \
  --network "$NETWORK" \
  --cap-drop ALL \
  --cap-add IPC_LOCK \
  --cap-add SETGID \
  --cap-add SETUID \
  --cap-add SYS_CHROOT \
  --cap-add SYS_RESOURCE \
  --volume "${KEY_VOLUME}:/root/.ssh" \
  "$IMAGE" >/dev/null

for _ in $(seq 1 30); do
  if output="$(
    docker run --rm \
      --network "$NETWORK" \
      --volume "${KEY_VOLUME}:/keys:ro" \
      --entrypoint bash \
      "$IMAGE" -lc \
      "ssh -T -p 2222 \
        -i /keys/id_ed25519 \
        -o BatchMode=yes \
        -o IdentitiesOnly=yes \
        -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=2 \
        root@${WORKER} \
        'printf NCCL_RDMA_SSH_COMMAND_OK'"
  )" && [[ "$output" == "NCCL_RDMA_SSH_COMMAND_OK" ]]; then
    exit 0
  fi
  sleep 1
done

docker logs "$WORKER" >&2 || true
echo "authenticated SSH command smoke failed under the MPIJob capability set" >&2
exit 1
