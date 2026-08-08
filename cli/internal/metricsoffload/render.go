// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metricsoffload renders the opt-in Stellar metrics sidecar.
//
// The sidecar argv below starts with "experiment offload metrics", which is a
// taugrid-portal verb — the tau binary does not provide it. This package stays
// in tau core because tau is what renders RayJobs, but the image supplied via
// --metrics-offload-image must be a taugrid-portal image. There is no default
// image and nothing in-repo sets one, so this is opt-in and cannot break an
// existing deployment silently; it is still a contract change worth knowing
// about when enabling the sidecar for the first time after the split.
package metricsoffload

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	RuntimeVolumeName = "tau-metrics-runtime"
	RuntimeMountPath  = "/var/run/tau"

	// SidecarCommand is the binary the sidecar execs. The verbs below belong
	// to taugrid-portal, whose image installs only this path — rendering
	// /usr/local/bin/tau here produces a container that fails at startup with
	// an exec error, long after the manifest looked correct.
	SidecarCommand = "/usr/local/bin/taugrid-portal"
)

type Mount struct {
	Name     string
	Path     string
	ReadOnly bool
	SubPath  string
}

func RuntimeVolume() map[string]any {
	return map[string]any{
		"name":     RuntimeVolumeName,
		"emptyDir": map[string]any{},
	}
}

func RuntimeMount() map[string]any {
	return map[string]any{"name": RuntimeVolumeName, "mountPath": RuntimeMountPath}
}

func BuildContainer(runtime Runtime, mounts []Mount) map[string]any {
	source := strings.TrimSpace(runtime.Source)
	if source == "" {
		source = DefaultSource
	}
	args := []any{
		"experiment", "offload", "metrics", "--watch",
		"--run", runtime.RunID,
		"--project", runtime.Project,
		"--experiment", runtime.Experiment,
		"--group", runtime.Group,
		"--source", source,
		"--out", runtime.Out,
		"--completion-file", runtime.CompletionFile,
		"--interval", runtime.Interval.String(),
		"--remote-write-endpoint", runtime.RemoteWriteEndpoint,
	}
	if runtime.BaselineExistingHistory {
		args = append(args, "--baseline-existing-history")
	}
	if runtime.ReadyFile != "" {
		args = append(args, "--ready-file", runtime.ReadyFile)
	}
	if runtime.DoneFile != "" {
		args = append(args, "--done-file", runtime.DoneFile)
	}
	for _, history := range runtime.History {
		args = append(args, "--history", history)
	}
	for _, tag := range TagArgs(runtime.Tags) {
		args = append(args, "--tag", tag)
	}
	if runtime.ArtifactURI != "" {
		args = append(args, "--status-artifact-uri", runtime.ArtifactURI)
	}
	if runtime.CheckpointURI != "" {
		args = append(args, "--status-checkpoint-uri", runtime.CheckpointURI)
	}

	volumeMounts := make([]any, 0, len(mounts)+1)
	for _, mount := range mounts {
		value := map[string]any{"name": mount.Name, "mountPath": mount.Path}
		if mount.ReadOnly {
			value["readOnly"] = true
		}
		if mount.SubPath != "" {
			value["subPath"] = mount.SubPath
		}
		volumeMounts = append(volumeMounts, value)
	}
	volumeMounts = append(volumeMounts, RuntimeMount())

	return map[string]any{
		"name":            "metrics-offload",
		"image":           runtime.Image,
		"imagePullPolicy": "IfNotPresent",
		"command":         []any{SidecarCommand},
		"args":            args,
		"env": []any{
			map[string]any{
				"name": "NODE_IP",
				"valueFrom": map[string]any{
					"fieldRef": map[string]any{"fieldPath": "status.hostIP"},
				},
			},
			map[string]any{"name": "TAU_EXP_STORE", "value": runtime.Store},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
			"limits":   map[string]any{"cpu": "500m", "memory": "512Mi"},
		},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"capabilities": map[string]any{
				"drop": []any{"ALL"},
			},
		},
		"volumeMounts": volumeMounts,
	}
}

func WrapCommand(command []string, runtime Runtime) ([]string, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("metrics offload requires a Tau-wrappable script or explicit command")
	}
	script, err := wrapperScript(runtime, `"$@" &`)
	if err != nil {
		return nil, err
	}
	return append([]string{"bash", "-c", script, "tau-metrics-entrypoint"}, command...), nil
}

func WrapShellScript(command string, runtime Runtime) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("metrics offload requires a non-empty entrypoint")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(command))
	launch := fmt.Sprintf(`tau_metrics_script=%s
printf '%%s' %s | base64 -d > "$tau_metrics_script"
chmod 0700 "$tau_metrics_script"
"$tau_metrics_script" &`, shellQuote(RuntimeMountPath+"/metrics-entrypoint.sh"), shellQuote(encoded))
	return wrapperScript(runtime, launch)
}

func wrapperScript(runtime Runtime, launch string) (string, error) {
	if err := runtime.Validate(); err != nil {
		return "", err
	}
	readyTimeoutSeconds := timeoutSeconds(runtime.ReadyTimeout, 120*time.Second)
	doneTimeoutSeconds := timeoutSeconds(runtime.DoneTimeout, DefaultDoneTimeout)
	return fmt.Sprintf(`tau_metrics_completion=%s
tau_metrics_ready=%s
tau_metrics_ready_timeout=%d
tau_metrics_done=%s
tau_metrics_done_timeout=%d
tau_metrics_child=""
tau_metrics_terminated=0
tau_metrics_status=0
tau_forward_metrics_signal() {
  tau_metrics_terminated=1
  if [ -n "${tau_metrics_child:-}" ]; then
    kill -TERM "$tau_metrics_child" 2>/dev/null || true
  fi
}
trap tau_forward_metrics_signal TERM INT
if [ -n "$tau_metrics_ready" ]; then
  tau_metrics_deadline=$(( $(date +%%s) + tau_metrics_ready_timeout ))
  while [ ! -f "$tau_metrics_ready" ]; do
    if [ "$tau_metrics_terminated" -eq 1 ]; then
      tau_metrics_status=143
      break
    fi
    if [ "$(date +%%s)" -ge "$tau_metrics_deadline" ]; then
      echo "metrics offloader did not become ready within ${tau_metrics_ready_timeout}s" >&2
      tau_metrics_status=124
      break
    fi
    sleep 1
  done
fi
if [ "$tau_metrics_status" -eq 0 ]; then
  %s
  tau_metrics_child=$!
  while :; do
    wait "$tau_metrics_child"
    tau_metrics_status=$?
    if ! kill -0 "$tau_metrics_child" 2>/dev/null; then
      break
    fi
  done
fi
trap - TERM INT
tau_metrics_state="succeeded"
tau_metrics_reason="workload-entrypoint-exit"
tau_metrics_message="entrypoint exited with code ${tau_metrics_status}"
if [ "$tau_metrics_terminated" -eq 1 ]; then
  tau_metrics_state="cancelled"
  tau_metrics_reason="workload-termination"
elif [ "$tau_metrics_status" -eq 124 ] && [ -n "$tau_metrics_ready" ] && [ ! -f "$tau_metrics_ready" ]; then
  tau_metrics_state="failed"
  tau_metrics_reason="metrics-offloader-not-ready"
  tau_metrics_message="metrics offloader did not become ready within ${tau_metrics_ready_timeout}s"
elif [ "$tau_metrics_status" -ne 0 ]; then
  tau_metrics_state="failed"
fi
tau_metrics_completed_at="$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
tau_metrics_tmp="${tau_metrics_completion}.tmp.$$"
mkdir -p "$(dirname "$tau_metrics_completion")"
printf '{"state":"%%s","reason":"%%s","message":"%%s","completed_at":"%%s"}\n' \
  "$tau_metrics_state" "$tau_metrics_reason" "$tau_metrics_message" "$tau_metrics_completed_at" > "$tau_metrics_tmp"
mv -f "$tau_metrics_tmp" "$tau_metrics_completion"
if [ -n "$tau_metrics_done" ]; then
  tau_metrics_deadline=$(( $(date +%%s) + tau_metrics_done_timeout ))
  while [ ! -f "$tau_metrics_done" ]; do
    if [ "$(date +%%s)" -ge "$tau_metrics_deadline" ]; then
      echo "metrics offloader did not confirm terminal publication within ${tau_metrics_done_timeout}s" >&2
      if [ "$tau_metrics_status" -eq 0 ]; then
        tau_metrics_status=125
      fi
      break
    fi
    sleep 1
  done
fi
exit "$tau_metrics_status"
`, shellQuote(runtime.CompletionFile), shellQuote(runtime.ReadyFile), readyTimeoutSeconds,
		shellQuote(runtime.DoneFile), doneTimeoutSeconds, launch), nil
}

func timeoutSeconds(value, fallback time.Duration) int64 {
	if value <= 0 {
		value = fallback
	}
	return int64((value + time.Second - 1) / time.Second)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
