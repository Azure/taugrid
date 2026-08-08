// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package raylogoffload

import "fmt"

const (
	AnnotationKey        = "adx-mon/log-destination"
	AnnotationValue      = "Logs:ContainerLogs"
	SidecarContainerName = "ray-driver-log-offload"
	PrepareInitName      = "prepare-ray-tmp"
	VolumeName           = "ray-tmp"
	VolumeMountPath      = "/tmp/ray"
	CompletionFilePath   = "/tmp/ray/tau-driver-complete"
	DefaultPollSeconds   = "1"
	DefaultDrainSeconds  = "5"
)

const PrepareScript = `mkdir -p /tmp/ray && chmod 1777 /tmp/ray`

const Script = `set -euo pipefail

ray_root="${TAU_RAY_TMP:-/tmp/ray}"
poll_seconds="${TAU_RAY_LOG_POLL_SECONDS:-1}"
completion_file="${TAU_RAY_LOG_COMPLETION_FILE:-/tmp/ray/tau-driver-complete}"
drain_seconds="${TAU_RAY_LOG_DRAIN_SECONDS:-5}"
seen_paths=""
tail_pids=()

canonical_path() {
  local path="$1"
  local dir base
  dir="$(cd -P "$(dirname "$path")" && pwd)"
  base="$(basename "$path")"
  printf '%s/%s\n' "$dir" "$base"
}

has_seen() {
  local needle="$1"
  local seen
  while IFS= read -r seen; do
    [[ "$seen" == "$needle" ]] && return 0
  done <<< "$seen_paths"
  return 1
}

start_tail() {
  local real="$1"
  seen_paths="${seen_paths}${real}"$'\n'
  tail -n +1 -F "$real" &
  tail_pids+=("$!")
}

discover_logs() {
  local entry candidate real
  local -a roots=("$ray_root/session_latest/logs")

  # Default contract: tail /tmp/ray/session_latest/logs/job-driver-*.log,
  # plus concrete session_* directories so session_latest flips do not drop logs.
  shopt -s nullglob
  for entry in "$ray_root"/session_*/logs; do
    [[ -d "$entry" ]] && roots+=("$entry")
  done
  shopt -u nullglob

  for entry in "${roots[@]}"; do
    [[ -d "$entry" ]] || continue
    shopt -s nullglob
    for candidate in "$entry"/job-driver-*.log; do
      [[ -f "$candidate" ]] || continue
      real="$(canonical_path "$candidate")"
      if has_seen "$real"; then
        continue
      fi
      start_tail "$real"
    done
    shopt -u nullglob
  done
}

cleanup() {
  local pid
  for pid in "${tail_pids[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait || true
}

trap cleanup EXIT
trap 'exit 0' INT TERM

while true; do
  discover_logs
  if [[ -f "$completion_file" ]]; then
    # The entrypoint writes the marker only after user code and synchronous
    # artifact finalization exit. Keep existing tails alive briefly so their
    # final appended bytes reach stdout, then stop holding the head pod open.
    sleep "$drain_seconds"
    discover_logs
    sleep "$poll_seconds"
    break
  fi
  sleep "$poll_seconds"
done
`

const CompletionSetupScript = `TAU_RAY_LOG_COMPLETION_FILE="${TAU_RAY_LOG_COMPLETION_FILE:-/tmp/ray/tau-driver-complete}"
tau_write_driver_log_completion() {
  tau_driver_status="$1"
  tau_driver_tmp="${TAU_RAY_LOG_COMPLETION_FILE}.tmp.$$"
  (mkdir -p "$(dirname "$TAU_RAY_LOG_COMPLETION_FILE")" &&
    printf '{"exit_code":%s}\n' "$tau_driver_status" > "$tau_driver_tmp" &&
    mv -f "$tau_driver_tmp" "$TAU_RAY_LOG_COMPLETION_FILE") || true
}
tau_complete_driver_logs() {
  tau_driver_status="$?"
  trap - EXIT
  tau_write_driver_log_completion "$tau_driver_status"
  exit "$tau_driver_status"
}`

// WrapShellScript records completion only after every nested lifecycle wrapper
// (metrics offload and artifact publication) has finished.
func WrapShellScript(command string) string {
	return fmt.Sprintf(`set +e
%s
set -m
tau_driver_child=""
tau_driver_forward_signal() {
  if [ -n "${tau_driver_child:-}" ]; then
    kill -TERM -- "-$tau_driver_child" 2>/dev/null || true
  fi
}
trap tau_complete_driver_logs EXIT
trap tau_driver_forward_signal TERM INT
(
%s
) &
tau_driver_child="$!"
while :; do
  wait "$tau_driver_child"
  tau_driver_status="$?"
  if ! kill -0 "$tau_driver_child" 2>/dev/null; then
    break
  fi
done
trap - TERM INT
set +m
exit "$tau_driver_status"
`, CompletionSetupScript, command)
}

func HeadPodAnnotations(base map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+1)
	for key, value := range base {
		merged[key] = value
	}
	if _, exists := merged[AnnotationKey]; !exists {
		merged[AnnotationKey] = AnnotationValue
	}
	return merged
}

func Volume() map[string]any {
	return map[string]any{
		"name":     VolumeName,
		"emptyDir": map[string]any{},
	}
}

func VolumeMount(readOnly bool) map[string]any {
	return map[string]any{
		"name":      VolumeName,
		"mountPath": VolumeMountPath,
		"readOnly":  readOnly,
	}
}

func SidecarContainer(image string) map[string]any {
	return map[string]any{
		"name":    SidecarContainerName,
		"image":   image,
		"command": []any{"/bin/bash", "-lc", "--"},
		"args":    []any{Script},
		"env": []any{
			map[string]any{"name": "TAU_RAY_LOG_COMPLETION_FILE", "value": CompletionFilePath},
			map[string]any{"name": "TAU_RAY_LOG_DRAIN_SECONDS", "value": DefaultDrainSeconds},
		},
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":    "10m",
				"memory": "32Mi",
			},
			"limits": map[string]any{
				"cpu":    "100m",
				"memory": "128Mi",
			},
		},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities": map[string]any{
				"drop": []any{"ALL"},
			},
			"readOnlyRootFilesystem": true,
			"runAsNonRoot":           true,
			"runAsUser":              int64(65532),
			"runAsGroup":             int64(65532),
			"seccompProfile": map[string]any{
				"type": "RuntimeDefault",
			},
		},
		"volumeMounts": []any{
			VolumeMount(true),
		},
	}
}

func PrepareInitContainer(image string) map[string]any {
	return map[string]any{
		"name":    PrepareInitName,
		"image":   image,
		"command": []any{"/bin/sh", "-c", PrepareScript},
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":    "10m",
				"memory": "32Mi",
			},
			"limits": map[string]any{
				"cpu":    "50m",
				"memory": "64Mi",
			},
		},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities": map[string]any{
				"drop": []any{"ALL"},
			},
			"readOnlyRootFilesystem": true,
			"runAsUser":              int64(0),
			"runAsGroup":             int64(0),
			"seccompProfile": map[string]any{
				"type": "RuntimeDefault",
			},
		},
		"volumeMounts": []any{
			VolumeMount(false),
		},
	}
}
