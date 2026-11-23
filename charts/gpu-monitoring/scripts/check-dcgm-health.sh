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

if status_result="$(printf '%s\n' "${output}" | awk -F "|" '
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

  left = tolower(trim($2))
  right = tolower(trim($3))
  if (left == "overall health") {
    status = right
  } else if (right ~ /^overall health:[[:space:]]*/) {
    sub(/^overall health:[[:space:]]*/, "", right)
    status = right
  } else {
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
  if (healthy == 1) {
    status = "Healthy"
  } else if (warning == 1) {
    status = "Warning"
  } else if (failure == 1) {
    status = "Failure"
  } else {
    status = "Unknown"
  }
  printf "%s rows=%d Healthy=%d Warning=%d Failure=%d Unknown=%d Malformed=%d", status, rows, healthy, warning, failure, unknown, malformed
  exit !(rows == 1 && healthy + warning + failure == 1 && unknown == 0 && malformed == 0)
}')"; then
  :
else
  echo "dcgm health check rejected Overall Health result set: ${status_result#* }"
  exit $NONOK
fi

health_status="${status_result%% *}"
status_summary="${status_result#* }"
case "${health_status}" in
  Healthy)
    ;;
  Warning)
    if warning_summary="$(printf '%s\n' "${output}" | awk -F "|" '
function trim(value) {
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
  return value
}
function normalize(value) {
  gsub(/[[:space:]]+/, " ", value)
  return trim(value)
}
function finish_incident() {
  if (incident == "") {
    if (incident_system != "") {
      malformed++
    }
    incident_system = ""
    return
  }
  incident = normalize(incident)
  incidents++
  if (incident !~ /^Detected XID 43 for GPU [0-9]+(, Detected XID 43 for GPU [0-9]+)*$/) {
    rejected++
  }
  incident = ""
  incident_system = ""
}
BEGIN {
  groups = 0
  entities = 0
  incidents = 0
  rejected = 0
  malformed = 0
  entity = 0
  errors = 0
}
{
  raw = trim($0)
  if (raw == "Health Monitor Report" || raw ~ /^\+[+=-]+\+[+=-]+\+$/) {
    next
  }
  if (NF == 3 && trim($1) == "" && trim($2) == "Health Monitor Report" && trim($3) == "") {
    next
  }
  if (NF != 4 || trim($1) != "" || trim($4) != "") {
    malformed++
    next
  }

  left = trim($2)
  right = trim($3)

  if (tolower(left) == "overall health" || tolower(right) ~ /^overall health:[[:space:]]*/) {
    finish_incident()
    next
  }

  if (left != "" && right == "" && left !~ /^-> /) {
    finish_incident()
    groups++
    entity = 0
    errors = 0
    next
  }

  if (left ~ /^-> [0-9]+$/) {
    finish_incident()
    if (right == "Warning") {
      entities++
      entity = 1
      errors = 0
    } else {
      malformed++
    }
    next
  }

  if (left == "-> Errors") {
    finish_incident()
    if (right == "" && entity) {
      errors = 1
    } else {
      malformed++
    }
    next
  }

  if (left ~ /^-> /) {
    finish_incident()
    if (right == "Warning" && entity && errors) {
      incident_system = left
    } else {
      malformed++
    }
    next
  }

  if (left == "" && right != "" && incident_system != "") {
    incident = incident " " right
    next
  }

  if (left != "" && right == "Warning") {
    finish_incident()
    entities++
    entity = 1
    errors = 1
    next
  }

  if (left == "" && right ~ /^[^:]+: Warning - / && entity) {
    finish_incident()
    sub(/^[^:]+: Warning - /, "", right)
    incident_system = "flat"
    incident = right
    next
  }

  if (left != "" || right != "") {
    malformed++
  }
}
END {
  finish_incident()
  printf "groups=%d entities=%d incidents=%d rejected=%d malformed=%d", groups, entities, incidents, rejected, malformed
  exit !(entities > 0 && incidents > 0 && rejected == 0 && malformed == 0)
}')"; then
      echo "dcgm health warning tolerated: all incidents are retained XID 43 application faults (${warning_summary})"
    else
      echo "dcgm health check rejected Warning incident set: ${status_summary}; ${warning_summary}"
      exit $NONOK
    fi
    ;;
  *)
    echo "dcgm health check rejected Overall Health result set: ${status_summary}"
    exit $NONOK
    ;;
esac

bash "${SCRIPT_DIR}/check-dcgm-watches.sh"
echo "${output}"
exit $OK
