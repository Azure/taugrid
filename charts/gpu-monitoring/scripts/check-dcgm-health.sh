#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2
readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ "${NPD_DCGM_REQUIRED:-0}" != "1" ]]; then
  echo "dcgm host diagnostic not applicable for this profile; exporter availability is reported separately"
  exit $UNKNOWN
fi

if ! command -v dcgmi >/dev/null 2>&1; then
  echo "dcgmi not found"
  exit 1
fi

if output="$(LC_ALL=C dcgmi health -c 2>&1)"; then
  health_rc=$OK
else
  health_rc=$?
fi
if [[ $health_rc -ne $OK ]]; then
  if [[ -n "${output}" ]]; then
    printf '%s\n' "${output}" | awk '
    {
      line = tolower($0)
      if (line !~ /overall[[:space:]]+health/) {
        print
      }
    }'
  fi
  echo "dcgmi health check failed with return code ${health_rc}"
  exit $NONOK
fi
if [[ -z "${output}" ]]; then
  echo "dcgm health check returned no result"
  exit $NONOK
fi

if status_summary="$(printf '%s\n' "${output}" | awk -F "|" '
function trim(value) {
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
  return value
}
BEGIN {
  rows = 0
  healthy = 0
  warning = 0
  failure = 0
  unknown = 0
  malformed = 0
}
{
  line = tolower($0)
  if (line !~ /overall[[:space:]]+health/) {
    next
  }

  if (NF != 4 || trim($1) != "" || trim($4) != "") {
    malformed++
    next
  }

  label = tolower(trim($2))
  status = tolower(trim($3))
  if (label != "overall health") {
    malformed++
    next
  }

  rows++
  if (status == "healthy") {
    healthy++
  } else if (status == "warning") {
    warning++
  } else if (status == "failure") {
    failure++
  } else {
    unknown++
  }
}
END {
  printf "rows=%d Healthy=%d Warning=%d Failure=%d Unknown=%d Malformed=%d", rows, healthy, warning, failure, unknown, malformed
  exit !(rows == 1 && healthy == 1 && warning == 0 && failure == 0 && unknown == 0 && malformed == 0)
}')"; then
  :
else
  echo "dcgm health check rejected Overall Health result set: ${status_summary}"
  exit $NONOK
fi

bash "${SCRIPT_DIR}/check-dcgm-watches.sh"
echo "${output}"
exit $OK
