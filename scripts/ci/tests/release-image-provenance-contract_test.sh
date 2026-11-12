#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly RELEASE_VERSION="1.2.3"
readonly SOURCE_COMMIT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

for image in tau taugrid-portal tau-core-controller taugrid-metrics-collector; do
  directory="${REPO_ROOT}/images/${image}"
  command="$(
    make --no-print-directory -C "$directory" -n docker-push \
      IMG="example.invalid/${image}:${RELEASE_VERSION}" \
      TAG="$RELEASE_VERSION" \
      VERSION="$RELEASE_VERSION" \
      COMMIT="$SOURCE_COMMIT" \
      PLATFORMS=linux/amd64
  )"
  grep -Fq -- "--build-arg VERSION=${RELEASE_VERSION}" <<<"$command"
  grep -Fq -- "--build-arg COMMIT=${SOURCE_COMMIT}" <<<"$command"
  grep -Fq 'org.opencontainers.image.version="${VERSION}"' "$directory/Dockerfile"
  grep -Fq 'org.opencontainers.image.revision="${COMMIT}"' "$directory/Dockerfile"
done

echo "Release image provenance contract tests passed"
